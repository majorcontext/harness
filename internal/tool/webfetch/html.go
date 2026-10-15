package webfetch

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipped are the elements whose content the reader never sees or that only
// navigate: scripts, styles, frames, embedded objects, navigation, and footers.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Svg: true,
	atom.Canvas: true, atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Head: true,
	atom.Nav: true, atom.Footer: true, atom.Link: true, atom.Meta: true, atom.Select: true, atom.Button: true,
}

// inline lists the elements that flow inside a line of text.
var inline = map[atom.Atom]bool{
	atom.A: true, atom.Abbr: true, atom.B: true, atom.Bdi: true, atom.Bdo: true, atom.Cite: true, atom.Code: true,
	atom.Data: true, atom.Del: true, atom.Dfn: true, atom.Em: true, atom.Font: true, atom.I: true, atom.Img: true,
	atom.Ins: true, atom.Kbd: true, atom.Label: true, atom.Mark: true, atom.Q: true, atom.S: true, atom.Samp: true,
	atom.Small: true, atom.Span: true, atom.Strong: true, atom.Sub: true, atom.Sup: true, atom.Time: true,
	atom.U: true, atom.Var: true, atom.Wbr: true, atom.Br: true,
}

// maxDepth is the nesting at which the converter stops to descend and reads
// the text of the subtree, so the stack and the prefixes stay bounded.
const maxDepth = 48

// page is the result of the conversion of one HTML document.
type page struct {
	Title string
	Body  string
	// Truncated says that the converter stopped at the byte bound.
	Truncated bool
}

// convertHTML reads an HTML document and returns its title and its readable
// content as markdown, or as plain text. base resolves relative links. The
// converter writes at most max bytes, and stops with the error of ctx.
func convertHTML(ctx context.Context, r io.Reader, base *url.URL, markdown bool, max int) (page, error) {
	if err := ctx.Err(); err != nil {
		return page{}, err
	}
	doc, err := html.Parse(r)
	if err != nil {
		return page{}, err
	}
	c := &converter{ctx: ctx, base: base, markdown: markdown, max: max, live: liveNodes(doc)}
	c.pageTitle = c.findTitle(doc)
	c.blocks(doc, 0)
	if err := ctx.Err(); err != nil {
		return page{}, err
	}
	body := strings.TrimSpace(c.out.String())
	return page{Title: c.pageTitle, Body: strings.ToValidUTF8(body, "�"), Truncated: c.full}, nil
}

type converter struct {
	ctx       context.Context
	base      *url.URL
	markdown  bool
	pageTitle string
	max       int
	live      map[*html.Node]bool

	out  strings.Builder
	full bool
	// prefix starts each line. first, when set, starts the next line instead.
	prefix, first string
	// tight is above 0 inside a list item, where blocks follow with no blank line.
	tight int
	// fresh skips the separator before the first block of a quote.
	fresh bool
}

// stop reports that the output is full or ctx has ended.
func (c *converter) stop() bool {
	if !c.full && (c.out.Len() > c.max || c.ctx.Err() != nil) {
		c.full = true
	}
	return c.full
}

// liveNodes returns the nodes that hold something to show: a text, an image
// with a description, or a rule. It walks the tree once, without recursion.
func liveNodes(doc *html.Node) map[*html.Node]bool {
	var order []*html.Node
	stack := []*html.Node{doc}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if skip(n) {
			continue
		}
		order = append(order, n)
		for ch := n.LastChild; ch != nil; ch = ch.PrevSibling {
			stack = append(stack, ch)
		}
	}
	live := make(map[*html.Node]bool)
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		switch {
		case n.Type == html.TextNode && strings.TrimFunc(n.Data, isSpace) != "":
			live[n] = true
		case n.Type == html.ElementNode && n.DataAtom == atom.Hr, n.Type == html.ElementNode && n.DataAtom == atom.Img && strings.TrimSpace(attr(n, "alt")) != "":
			live[n] = true
		}
		if live[n] && n.Parent != nil {
			live[n.Parent] = true
		}
	}
	return live
}

func (c *converter) findTitle(doc *html.Node) string {
	stack := []*html.Node{doc}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Type == html.ElementNode && n.DataAtom == atom.Title {
			return strings.Join(strings.Fields(text(n)), " ")
		}
		for ch := n.LastChild; ch != nil; ch = ch.PrevSibling {
			stack = append(stack, ch)
		}
	}
	return ""
}

// text returns the text of a subtree, without recursion.
func text(n *html.Node) string {
	var b strings.Builder
	stack := []*html.Node{n}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
			continue
		}
		for ch := m.LastChild; ch != nil; ch = ch.PrevSibling {
			stack = append(stack, ch)
		}
	}
	return b.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hidden(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "hidden" || a.Key == "aria-hidden" && a.Val == "true" {
			return true
		}
	}
	return false
}

func skip(n *html.Node) bool {
	return n.Type == html.CommentNode || n.Type == html.DoctypeNode || n.Type == html.ElementNode && (skipped[n.DataAtom] || hidden(n))
}

// sep writes the blank line that sets a block apart from the one before it.
func (c *converter) sep() {
	if c.fresh {
		c.fresh = false
		return
	}
	if c.out.Len() == 0 || c.tight > 0 {
		return
	}
	c.out.WriteString(strings.TrimRight(c.prefix, " "))
	c.out.WriteByte('\n')
}

// emit writes s as lines that start with the current prefix.
func (c *converter) emit(s string) {
	c.fresh = false
	for line := range strings.SplitSeq(s, "\n") {
		p := c.prefix
		if c.first != "" {
			p, c.first = c.first, ""
		}
		if p != "" {
			line = strings.TrimRight(p+line, " ")
		}
		c.out.WriteString(line)
		c.out.WriteByte('\n')
	}
}

// put sets s apart from the block before it and writes it.
func (c *converter) put(s string) {
	if s != "" {
		c.sep()
		c.emit(s)
	}
}

// blocks writes the children of n as blocks.
func (c *converter) blocks(n *html.Node, depth int) {
	var run strings.Builder
	flush := func() {
		c.put(strings.TrimSpace(run.String()))
		run.Reset()
	}
	for ch := n.FirstChild; ch != nil && !c.stop(); ch = ch.NextSibling {
		switch {
		case skip(ch):
		case ch.Type == html.TextNode, ch.Type == html.ElementNode && inline[ch.DataAtom]:
			c.inline(&run, ch, depth)
		case !c.live[ch]:
		case depth > maxDepth:
			flush()
			c.put(strings.Join(strings.Fields(text(ch)), " "))
		case ch.Type == html.ElementNode:
			flush()
			c.block(ch, depth+1)
		default:
			flush()
			c.blocks(ch, depth+1)
		}
	}
	flush()
}

// block writes one block element.
func (c *converter) block(n *html.Node, depth int) {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		var b strings.Builder
		c.inline(&b, n, depth)
		s := strings.TrimSpace(b.String())
		if s != "" && c.markdown {
			s = strings.Repeat("#", int(n.DataAtom.String()[1]-'0')) + " " + s
		}
		c.put(s)
	case atom.Ul, atom.Ol:
		c.list(n, depth)
	case atom.Pre:
		c.put(c.pre(n))
	case atom.Blockquote:
		if !c.markdown {
			c.blocks(n, depth)
			return
		}
		c.sep()
		saved, savedFirst := c.prefix, c.first
		c.prefix += "> "
		if c.first != "" {
			c.first += "> "
		}
		c.fresh = true
		c.blocks(n, depth)
		c.fresh = false
		c.prefix, c.first = saved, savedFirst
	case atom.Hr:
		if c.markdown {
			c.put("---")
		}
	case atom.Table:
		c.put(c.table(n, depth))
	default:
		c.blocks(n, depth)
	}
}

func (c *converter) list(n *html.Node, depth int) {
	c.sep()
	num := 1
	if n.DataAtom == atom.Ol {
		if v, err := strconv.Atoi(attr(n, "start")); err == nil {
			num = v
		}
	}
	for ch := n.FirstChild; ch != nil && !c.stop(); ch = ch.NextSibling {
		if ch.Type != html.ElementNode || ch.DataAtom != atom.Li || skip(ch) || !c.live[ch] {
			continue
		}
		marker := "- "
		if n.DataAtom == atom.Ol {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		saved, savedFirst := c.prefix, c.first
		if c.first != "" {
			c.first += marker
		} else {
			c.first = c.prefix + marker
		}
		c.prefix += strings.Repeat(" ", len(marker))
		c.tight++
		c.blocks(ch, depth+1)
		c.tight--
		c.prefix, c.first = saved, savedFirst
	}
}

func (c *converter) pre(n *html.Node) string {
	body := strings.Trim(text(n), "\n")
	if !c.markdown {
		return body
	}
	lang := ""
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && ch.DataAtom == atom.Code {
			for _, class := range strings.Fields(attr(ch, "class")) {
				if l, ok := strings.CutPrefix(class, "language-"); ok {
					lang = l
				}
			}
		}
	}
	return "```" + lang + "\n" + body + "\n```"
}

func (c *converter) table(n *html.Node, depth int) string {
	var rows [][]string
	var walk func(*html.Node, int)
	walk = func(m *html.Node, d int) {
		for ch := m.FirstChild; ch != nil && d < maxDepth && !c.stop(); ch = ch.NextSibling {
			switch {
			case skip(ch):
			case ch.Type == html.ElementNode && ch.DataAtom == atom.Tr:
				var cells []string
				for cell := ch.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type == html.ElementNode && (cell.DataAtom == atom.Td || cell.DataAtom == atom.Th) && !skip(cell) {
						var b strings.Builder
						c.inline(&b, cell, depth)
						cells = append(cells, strings.ReplaceAll(strings.TrimSpace(b.String()), "|", `\|`))
					}
				}
				if len(cells) > 0 {
					rows = append(rows, cells)
				}
			case ch.Type == html.ElementNode && ch.DataAtom != atom.Table:
				walk(ch, d+1)
			}
		}
	}
	walk(n, 0)
	if len(rows) == 0 {
		return ""
	}
	var lines []string
	for i, row := range rows {
		if c.markdown {
			lines = append(lines, "| "+strings.Join(row, " | ")+" |")
			if i == 0 {
				lines = append(lines, "|"+strings.Repeat(" --- |", len(row)))
			}
		} else {
			lines = append(lines, strings.Join(row, "\t"))
		}
	}
	return strings.Join(lines, "\n")
}

// inline writes the content of n as one run of text. A run of white space
// becomes one space, and a block element inside a link or a span becomes a
// space.
func (c *converter) inline(b *strings.Builder, n *html.Node, depth int) {
	switch {
	case n.Type == html.TextNode:
		writeText(b, n.Data)
		return
	case n.Type != html.ElementNode || skip(n) || c.stop():
		return
	case depth > maxDepth:
		writeText(b, text(n))
		return
	}
	switch n.DataAtom {
	case atom.Br:
		b.WriteString("\n")
	case atom.Img:
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			if c.markdown {
				if src := c.resolve(attr(n, "src")); src != "" {
					b.WriteString("![" + alt + "](" + src + ")")
					return
				}
			}
			writeText(b, alt)
		}
	case atom.A:
		c.link(b, n, depth)
	case atom.Strong, atom.B:
		c.wrap(b, n, "**", depth)
	case atom.Em, atom.I:
		c.wrap(b, n, "*", depth)
	case atom.Code, atom.Kbd, atom.Samp:
		c.wrap(b, n, "`", depth)
	default:
		c.children(b, n, depth)
		if !inline[n.DataAtom] {
			b.WriteString(" ")
		}
	}
}

func (c *converter) children(b *strings.Builder, n *html.Node, depth int) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.inline(b, ch, depth+1)
	}
}

// wrap puts mark around the content of n in markdown, and leaves it bare in text.
func (c *converter) wrap(b *strings.Builder, n *html.Node, mark string, depth int) {
	var inner strings.Builder
	c.children(&inner, n, depth)
	s := strings.TrimSpace(inner.String())
	switch {
	case s == "":
	case !c.markdown:
		writeText(b, inner.String())
	default:
		if strings.HasPrefix(inner.String(), " ") {
			writeText(b, " ")
		}
		b.WriteString(mark + s + mark)
		if strings.HasSuffix(inner.String(), " ") {
			writeText(b, " ")
		}
	}
}

func (c *converter) link(b *strings.Builder, n *html.Node, depth int) {
	var inner strings.Builder
	c.children(&inner, n, depth)
	s := strings.TrimSpace(inner.String())
	href := c.resolve(attr(n, "href"))
	switch {
	case s == "":
	case !c.markdown || href == "":
		writeText(b, inner.String())
	default:
		if strings.HasPrefix(inner.String(), " ") {
			writeText(b, " ")
		}
		b.WriteString("[" + s + "](" + href + ")")
		if strings.HasSuffix(inner.String(), " ") {
			writeText(b, " ")
		}
	}
}

// resolve returns ref as an absolute http or https URL, or "" for a fragment,
// a script, a mail address, or any other scheme.
func (c *converter) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if c.base != nil {
		u = c.base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == 0xA0
}

// writeText appends s with each run of white space as one space, and no space
// at the start of a line.
func writeText(b *strings.Builder, s string) {
	words := strings.FieldsFunc(s, isSpace)
	cur := b.String()
	need := cur != "" && !strings.HasSuffix(cur, "\n") && !strings.HasSuffix(cur, " ")
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if s != "" && isSpace(first) && need {
		b.WriteByte(' ')
	}
	b.WriteString(strings.Join(words, " "))
	if len(words) > 0 && isSpace(last) {
		b.WriteByte(' ')
	}
}
