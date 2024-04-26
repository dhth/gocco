// **Gocco** is a Go port of [Docco](http://jashkenas.github.com/docco/): the
// original quick-and-dirty, hundred-line-long, literate-programming-style
// documentation generator. It produces HTML that displays your comments
// alongside your code. Comments are passed through
// [Markdown](http://daringfireball.net/projects/markdown/syntax), and code is
// highlighted using [chroma](https://github.com/alecthomas/chroma). This page
// is the result of running Gocco against its own source file.
//
// If you install Gocco, you can run it from the command-line:
//
// gocco *.go
//
// ...will generate an HTML documentation page for each of the named source
// files, with a menu linking to the other pages, saving it into a `docs`
// folder.
//
// The [source for Gocco](http://github.com/nikhilm/gocco) is available on
// GitHub, and released under the MIT license.
package main

import (
	"bytes"
	"container/list"
	"flag"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/russross/blackfriday"
)

// ## Types
// Due to Go's statically typed nature, what is passed around in object
// literals in Docco, requires various structures

// A `Section` captures a piece of documentation and code
// Every time interleaving code is found between two comments
// a new `Section` is created.
type Section struct {
	docsText []byte
	codeText []byte
	DocsHTML []byte
	CodeHTML []byte
}

// a `TemplateSection` is a section that can be passed
// to Go's templating system, which expects strings.
type TemplateSection struct {
	DocsHTML string
	CodeHTML string
	// The `Index` field is used to create anchors to sections
	Index int
}

// a `Language` describes a programming language
type Language struct {
	// `chroma` name of the language
	name string
	// The comment delimiter
	symbol string
	// The regular expression to match the comment delimiter
	commentMatcher *regexp.Regexp
}

// a `TemplateData` is per-file
type TemplateData struct {
	// Title of the HTML output
	Title string
	// The Sections making up this file
	Sections []*TemplateSection
	// A full list of source files so that a table-of-contents can
	// be generated
	Sources []string
	// Only generate the TOC is there is more than one file
	// Go's templating system does not allow expressions in the
	// template, so calculate it outside
	Multiple     bool
	StyleClasses string
}

// a map of all the languages we know
var languages map[string]*Language

// paths of all the source files, sorted
var sources []string

// absolute path to get resources
var packageLocation string

const (
	// Wrap the code in these
	highlightStart = "<div class=\"highlight\"><pre>"
	highlightEnd   = "</pre></div>"

	chromaStyle = "catppuccin-latte"
)

// ## Main documentation generation functions

// Generate the documentation for a single source file
// by splitting it into sections, highlighting each section
// and putting it together.
// The WaitGroup is used to signal we are done, so that the main
// goroutine waits for all the sub goroutines
func generateDocumentation(source string, wg *sync.WaitGroup) {
	code, err := os.ReadFile(source)
	if err != nil {
		log.Panic(err)
	}
	sections := parse(source, code)
	highlight(source, sections)
	generateHTML(source, sections)
	wg.Done()
}

// Parse splits code into `Section`s
func parse(source string, code []byte) *list.List {
	lines := bytes.Split(code, []byte("\n"))
	sections := new(list.List)
	sections.Init()
	language := getLanguage(source)

	var hasCode bool
	var codeText = new(bytes.Buffer)
	var docsText = new(bytes.Buffer)

	// save a new section
	save := func(docs, code []byte) {
		// deep copy the slices since slices always refer to the same storage
		// by default
		docsCopy, codeCopy := make([]byte, len(docs)), make([]byte, len(code))
		copy(docsCopy, docs)
		copy(codeCopy, code)
		sections.PushBack(&Section{docsCopy, codeCopy, nil, nil})
	}

	for _, line := range lines {
		// if the line is a comment
		if language.commentMatcher.Match(line) {
			// but there was previous code
			if hasCode {
				// we need to save the existing documentation and text
				// as a section and start a new section since code blocks
				// have to be delimited before being sent for syntax highlighting
				save(docsText.Bytes(), codeText.Bytes())
				hasCode = false
				codeText.Reset()
				docsText.Reset()
			}
			docsText.Write(language.commentMatcher.ReplaceAll(line, nil))
			docsText.WriteString("\n")
		} else {
			hasCode = true
			codeText.Write(line)
			codeText.WriteString("\n")
		}
	}
	// save any remaining parts of the source file
	save(docsText.Bytes(), codeText.Bytes())
	return sections
}

// `highlight` iterates through sections, and generates documentation
// and code HTML for each. Syntax highlights for code are added using
// chroma
func highlight(source string, sections *list.List) {
	language := getLanguage(source)
	lexer := lexers.Get(language.name)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	style := styles.Get(chromaStyle)
	if style == nil {
		style = styles.Fallback
	}
	formatter := html.New(html.WithClasses(true))

	for e := sections.Front(); e != nil; e = e.Next() {
		var buf bytes.Buffer
		iterator, err := lexer.Tokenise(nil, string(e.Value.(*Section).codeText))
		if err != nil {
			buf.Write(e.Value.(*Section).codeText)
			continue
		}
		err = formatter.Format(&buf, style, iterator)
		if err != nil {
			buf.Write(e.Value.(*Section).codeText)
		}
		e.Value.(*Section).CodeHTML = bytes.Join([][]byte{[]byte(highlightStart), []byte(highlightEnd)}, buf.Bytes())
		e.Value.(*Section).DocsHTML = blackfriday.MarkdownCommon(e.Value.(*Section).docsText)
	}
}

// compute the output location (in `docs/`) for the file
func destination(source string) string {
	base := filepath.Base(source)
	return "docs/" + base[0:strings.LastIndex(base, filepath.Ext(base))] + ".html"
}

// render the final HTML
func generateHTML(source string, sections *list.List) {
	title := filepath.Base(source)
	dest := destination(source)
	// convert every `Section` into corresponding `TemplateSection`
	sectionsArray := make([]*TemplateSection, sections.Len())
	for e, i := sections.Front(), 0; e != nil; e, i = e.Next(), i+1 {
		var sec = e.Value.(*Section)
		docsBuf := bytes.NewBuffer(sec.DocsHTML)
		codeBuf := bytes.NewBuffer(sec.CodeHTML)
		sectionsArray[i] = &TemplateSection{docsBuf.String(), codeBuf.String(), i + 1}
	}

	var styleBuf bytes.Buffer
	style := styles.Get(chromaStyle)
	if style == nil {
		style = styles.Fallback
	}
	formatter := html.New(html.WithClasses(true))
	_ = formatter.WriteCSS(&styleBuf, style)

	// run through the Go template
	html := goccoTemplate(TemplateData{title, sectionsArray, sources, len(sources) > 1, styleBuf.String()})
	log.Println("gocco: ", source, " -> ", dest)
	os.WriteFile(dest, html, 0644)
}

func goccoTemplate(data TemplateData) []byte {
	// this hack is required because `ParseFiles` doesn't
	// seem to work properly, always complaining about empty templates
	t, err := template.New("gocco").Funcs(
		// introduce the two functions that the template needs
		template.FuncMap{
			"base":        filepath.Base,
			"destination": destination,
		}).Parse(HTML)
	if err != nil {
		panic(err)
	}
	buf := new(bytes.Buffer)
	err = t.Execute(buf, data)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// get a `Language` given a path
func getLanguage(source string) *Language {
	return languages[filepath.Ext(source)]
}

// make sure `docs/` exists
func ensureDirectory(name string) {
	os.MkdirAll(name, 0755)
}

func setupLanguages() {
	languages = make(map[string]*Language)
	// you should add more languages here
	// only the first two fields should change, the rest should
	// be `nil `
	languages[".go"] = &Language{"go", "//", nil}
}

func setup() {
	setupLanguages()

	// create the regular expressions based on the language comment symbol
	for _, lang := range languages {
		lang.commentMatcher, _ = regexp.Compile("^\\s*" + lang.symbol + "\\s?")
	}
}

// let's Go!
func main() {
	setup()

	flag.Parse()
	sources = flag.Args()
	sort.Strings(sources)

	if flag.NArg() <= 0 {
		return
	}

	ensureDirectory("docs")
	os.WriteFile("docs/gocco.css", bytes.NewBufferString(Css).Bytes(), 0755)

	wg := new(sync.WaitGroup)
	wg.Add(flag.NArg())
	for _, arg := range flag.Args() {
		go generateDocumentation(arg, wg)
	}
	wg.Wait()
}
