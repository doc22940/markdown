package html

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/gomarkdown/markdown/ast"
)

// Flags control optional behavior of HTML renderer.
type Flags int

// HTML renderer configuration options.
const (
	FlagsNone               Flags = 0
	SkipHTML                Flags = 1 << iota // Skip preformatted HTML blocks
	SkipImages                                // Skip embedded images
	SkipLinks                                 // Skip all links
	Safelink                                  // Only link to trusted protocols
	NofollowLinks                             // Only link with rel="nofollow"
	NoreferrerLinks                           // Only link with rel="noreferrer"
	HrefTargetBlank                           // Add a blank target
	CompletePage                              // Generate a complete HTML page
	UseXHTML                                  // Generate XHTML output instead of HTML
	FootnoteReturnLinks                       // Generate a link at the end of a footnote to return to the source
	Smartypants                               // Enable smart punctuation substitutions
	SmartypantsFractions                      // Enable smart fractions (with Smartypants)
	SmartypantsDashes                         // Enable smart dashes (with Smartypants)
	SmartypantsLatexDashes                    // Enable LaTeX-style dashes (with Smartypants)
	SmartypantsAngledQuotes                   // Enable angled double quotes (with Smartypants) for double quotes rendering
	SmartypantsQuotesNBSP                     // Enable « French guillemets » (with Smartypants)
	TOC                                       // Generate a table of contents

	CommonFlags Flags = Smartypants | SmartypantsFractions | SmartypantsDashes | SmartypantsLatexDashes
)

var (
	htmlTagRe = regexp.MustCompile("(?i)^" + htmlTag)
)

const (
	htmlTag = "(?:" + openTag + "|" + closeTag + "|" + htmlComment + "|" +
		processingInstruction + "|" + declaration + "|" + cdata + ")"
	closeTag              = "</" + tagName + "\\s*[>]"
	openTag               = "<" + tagName + attribute + "*" + "\\s*/?>"
	attribute             = "(?:" + "\\s+" + attributeName + attributeValueSpec + "?)"
	attributeValue        = "(?:" + unquotedValue + "|" + singleQuotedValue + "|" + doubleQuotedValue + ")"
	attributeValueSpec    = "(?:" + "\\s*=" + "\\s*" + attributeValue + ")"
	attributeName         = "[a-zA-Z_:][a-zA-Z0-9:._-]*"
	cdata                 = "<!\\[CDATA\\[[\\s\\S]*?\\]\\]>"
	declaration           = "<![A-Z]+" + "\\s+[^>]*>"
	doubleQuotedValue     = "\"[^\"]*\""
	htmlComment           = "<!---->|<!--(?:-?[^>-])(?:-?[^-])*-->"
	processingInstruction = "[<][?].*?[?][>]"
	singleQuotedValue     = "'[^']*'"
	tagName               = "[A-Za-z][A-Za-z0-9-]*"
	unquotedValue         = "[^\"'=<>`\\x00-\\x20]+"
)

// RenderNodeFunc allows reusing most of Renderer logic and replacing
// rendering of some nodes. If it returns false, Renderer.RenderNode
// will execute its logic. If it returns true, Renderer.RenderNode will
// skip rendering this node and will return WalkStatus
type RenderNodeFunc func(w io.Writer, node *ast.Node, entering bool) (ast.WalkStatus, bool)

// RendererOptions is a collection of supplementary parameters tweaking
// the behavior of various parts of HTML renderer.
type RendererOptions struct {
	// Prepend this text to each relative URL.
	AbsolutePrefix string
	// Add this text to each footnote anchor, to ensure uniqueness.
	FootnoteAnchorPrefix string
	// Show this text inside the <a> tag for a footnote return link, if the
	// HTML_FOOTNOTE_RETURN_LINKS flag is enabled. If blank, the string
	// <sup>[return]</sup> is used.
	FootnoteReturnLinkContents string
	// If set, add this text to the front of each Heading ID, to ensure
	// uniqueness.
	HeadingIDPrefix string
	// If set, add this text to the back of each Heading ID, to ensure uniqueness.
	HeadingIDSuffix string

	Title string // Document title (used if CompletePage is set)
	CSS   string // Optional CSS file URL (used if CompletePage is set)
	Icon  string // Optional icon file URL (used if CompletePage is set)

	Flags Flags // Flags allow customizing this renderer's behavior

	// if set, called at the start of RenderNode(). Allows replacing
	// rendering of some nodes
	RenderNodeHook RenderNodeFunc
}

// Renderer implements Renderer interface for HTML output.
//
// Do not create this directly, instead use the NewRenderer function.
type Renderer struct {
	opts RendererOptions

	closeTag string // how to end singleton tags: either " />" or ">"

	// Track heading IDs to prevent ID collision in a single generation.
	headingIDs map[string]int

	lastOutputLen int
	disableTags   int

	sr *SPRenderer
}

// NewRenderer creates and configures an Renderer object, which
// satisfies the Renderer interface.
func NewRenderer(opts RendererOptions) *Renderer {
	// configure the rendering engine
	closeTag := ">"
	if opts.Flags&UseXHTML != 0 {
		closeTag = " />"
	}

	if opts.FootnoteReturnLinkContents == "" {
		opts.FootnoteReturnLinkContents = `<sup>[return]</sup>`
	}

	return &Renderer{
		opts: opts,

		closeTag:   closeTag,
		headingIDs: make(map[string]int),

		sr: NewSmartypantsRenderer(opts.Flags),
	}
}

func isHTMLTag(tag []byte, tagname string) bool {
	found, _ := findHTMLTagPos(tag, tagname)
	return found
}

// Look for a character, but ignore it when it's in any kind of quotes, it
// might be JavaScript
func skipUntilCharIgnoreQuotes(html []byte, start int, char byte) int {
	inSingleQuote := false
	inDoubleQuote := false
	inGraveQuote := false
	i := start
	for i < len(html) {
		switch {
		case html[i] == char && !inSingleQuote && !inDoubleQuote && !inGraveQuote:
			return i
		case html[i] == '\'':
			inSingleQuote = !inSingleQuote
		case html[i] == '"':
			inDoubleQuote = !inDoubleQuote
		case html[i] == '`':
			inGraveQuote = !inGraveQuote
		}
		i++
	}
	return start
}

func findHTMLTagPos(tag []byte, tagname string) (bool, int) {
	i := 0
	if i < len(tag) && tag[0] != '<' {
		return false, -1
	}
	i++
	i = skipSpace(tag, i)

	if i < len(tag) && tag[i] == '/' {
		i++
	}

	i = skipSpace(tag, i)
	j := 0
	for ; i < len(tag); i, j = i+1, j+1 {
		if j >= len(tagname) {
			break
		}

		if strings.ToLower(string(tag[i]))[0] != tagname[j] {
			return false, -1
		}
	}

	if i == len(tag) {
		return false, -1
	}

	rightAngle := skipUntilCharIgnoreQuotes(tag, i, '>')
	if rightAngle >= i {
		return true, rightAngle
	}

	return false, -1
}

func isRelativeLink(link []byte) (yes bool) {
	// a tag begin with '#'
	if link[0] == '#' {
		return true
	}

	// link begin with '/' but not '//', the second maybe a protocol relative link
	if len(link) >= 2 && link[0] == '/' && link[1] != '/' {
		return true
	}

	// only the root '/'
	if len(link) == 1 && link[0] == '/' {
		return true
	}

	// current directory : begin with "./"
	if bytes.HasPrefix(link, []byte("./")) {
		return true
	}

	// parent directory : begin with "../"
	if bytes.HasPrefix(link, []byte("../")) {
		return true
	}

	return false
}

func (r *Renderer) ensureUniqueHeadingID(id string) string {
	for count, found := r.headingIDs[id]; found; count, found = r.headingIDs[id] {
		tmp := fmt.Sprintf("%s-%d", id, count+1)

		if _, tmpFound := r.headingIDs[tmp]; !tmpFound {
			r.headingIDs[id] = count + 1
			id = tmp
		} else {
			id = id + "-1"
		}
	}

	if _, found := r.headingIDs[id]; !found {
		r.headingIDs[id] = 0
	}

	return id
}

func (r *Renderer) addAbsPrefix(link []byte) []byte {
	if r.opts.AbsolutePrefix != "" && isRelativeLink(link) && link[0] != '.' {
		newDest := r.opts.AbsolutePrefix
		if link[0] != '/' {
			newDest += "/"
		}
		newDest += string(link)
		return []byte(newDest)
	}
	return link
}

func appendLinkAttrs(attrs []string, flags Flags, link []byte) []string {
	if isRelativeLink(link) {
		return attrs
	}
	var val []string
	if flags&NofollowLinks != 0 {
		val = append(val, "nofollow")
	}
	if flags&NoreferrerLinks != 0 {
		val = append(val, "noreferrer")
	}
	if flags&HrefTargetBlank != 0 {
		attrs = append(attrs, `target="_blank"`)
	}
	if len(val) == 0 {
		return attrs
	}
	attr := fmt.Sprintf("rel=%q", strings.Join(val, " "))
	return append(attrs, attr)
}

func isMailto(link []byte) bool {
	return bytes.HasPrefix(link, []byte("mailto:"))
}

func needSkipLink(flags Flags, dest []byte) bool {
	if flags&SkipLinks != 0 {
		return true
	}
	return flags&Safelink != 0 && !isSafeLink(dest) && !isMailto(dest)
}

func isSmartypantable(node *ast.Node) bool {
	switch node.Parent.Data.(type) {
	case *ast.LinkData, *ast.CodeBlockData, *ast.CodeData:
		return false
	}
	return true
}

func appendLanguageAttr(attrs []string, info []byte) []string {
	if len(info) == 0 {
		return attrs
	}
	endOfLang := bytes.IndexAny(info, "\t ")
	if endOfLang < 0 {
		endOfLang = len(info)
	}
	s := `class="language-` + string(info[:endOfLang]) + `"`
	return append(attrs, s)
}

func (r *Renderer) outTag(w io.Writer, name string, attrs []string) {
	var s string
	if len(attrs) > 0 {
		s = " " + strings.Join(attrs, " ")
	}
	io.WriteString(w, name+s+">")
	r.lastOutputLen = 1
}

func footnoteRef(prefix string, node *ast.LinkData) string {
	urlFrag := prefix + string(slugify(node.Destination))
	nStr := strconv.Itoa(node.NoteID)
	anchor := `<a rel="footnote" href="#fn:` + urlFrag + `">` + nStr + `</a>`
	return `<sup class="footnote-ref" id="fnref:` + urlFrag + `">` + anchor + `</sup>`
}

func footnoteItem(prefix string, slug []byte) string {
	return `<li id="fn:` + prefix + string(slug) + `">`
}

func footnoteReturnLink(prefix, returnLink string, slug []byte) string {
	return ` <a class="footnote-return" href="#fnref:` + prefix + string(slug) + `">` + returnLink + `</a>`
}

func itemOpenCR(node *ast.Node) bool {
	if node.Prev() == nil {
		return false
	}
	ld := node.Parent.Data.(*ast.ListData)
	return !ld.Tight && ld.ListFlags&ast.ListTypeDefinition == 0
}

func skipParagraphTags(node *ast.Node) bool {
	parent := node.Parent
	grandparent := parent.Parent
	if grandparent == nil || !isListData(grandparent.Data) {
		return false
	}
	isParentTerm := isListItemTerm(parent)
	grandparentListData := grandparent.Data.(*ast.ListData)
	tightOrTerm := grandparentListData.Tight || isParentTerm
	return tightOrTerm
}

// TODO: change this to be ast.CellAlignFlags.ToString()
func cellAlignment(align ast.CellAlignFlags) string {
	switch align {
	case ast.TableAlignmentLeft:
		return "left"
	case ast.TableAlignmentRight:
		return "right"
	case ast.TableAlignmentCenter:
		return "center"
	default:
		return ""
	}
}

func (r *Renderer) out(w io.Writer, d []byte) {
	r.lastOutputLen = len(d)
	if r.disableTags > 0 {
		d = htmlTagRe.ReplaceAll(d, []byte{})
	}
	w.Write(d)
}

func (r *Renderer) outs(w io.Writer, s string) {
	r.lastOutputLen = len(s)
	if r.disableTags > 0 {
		s = htmlTagRe.ReplaceAllString(s, "")
	}
	io.WriteString(w, s)
}

func (r *Renderer) cr(w io.Writer) {
	if r.lastOutputLen > 0 {
		r.outs(w, "\n")
	}
}

var (
	openHTags  = []string{"<h1", "<h2", "<h3", "<h4", "<h5"}
	closeHTags = []string{"</h1>", "</h2>", "</h3>", "</h4>", "</h5>"}
)

func headingOpenTagFromLevel(level int) string {
	if level < 1 || level > 5 {
		return "<h6"
	}
	return openHTags[level-1]
}

func headingCloseTagFromLevel(level int) string {
	if level < 1 || level > 5 {
		return "</h6>"
	}
	return closeHTags[level-1]
}

func (r *Renderer) outHRTag(w io.Writer) {
	r.outOneOf(w, r.opts.Flags&UseXHTML == 0, "<hr>", "<hr />")
}

func (r *Renderer) text(w io.Writer, node *ast.Node, nodeData *ast.TextData) {
	if r.opts.Flags&Smartypants != 0 {
		var tmp bytes.Buffer
		EscapeHTML(&tmp, node.Literal)
		r.sr.Process(w, tmp.Bytes())
	} else {
		if isLinkData(node.Parent.Data) {
			escLink(w, node.Literal)
		} else {
			EscapeHTML(w, node.Literal)
		}
	}
}

func (r *Renderer) hardBreak(w io.Writer, node *ast.Node, nodeData *ast.HardbreakData) {
	r.outOneOf(w, r.opts.Flags&UseXHTML == 0, "<br>", "<br />")
	r.cr(w)
}

func (r *Renderer) outOneOf(w io.Writer, outFirst bool, first string, second string) {
	if outFirst {
		r.outs(w, first)
	} else {
		r.outs(w, second)
	}
}

func (r *Renderer) outOneOfCr(w io.Writer, outFirst bool, first string, second string) {
	if outFirst {
		r.cr(w)
		r.outs(w, first)
	} else {
		r.outs(w, second)
		r.cr(w)
	}
}

func (r *Renderer) span(w io.Writer, node *ast.Node, nodeData *ast.HTMLSpanData) {
	if r.opts.Flags&SkipHTML == 0 {
		r.out(w, node.Literal)
	}
}

func (r *Renderer) linkEnter(w io.Writer, node *ast.Node, nodeData *ast.LinkData) {
	var attrs []string
	dest := nodeData.Destination
	dest = r.addAbsPrefix(dest)
	var hrefBuf bytes.Buffer
	hrefBuf.WriteString("href=\"")
	escLink(&hrefBuf, dest)
	hrefBuf.WriteByte('"')
	attrs = append(attrs, hrefBuf.String())
	if nodeData.NoteID != 0 {
		r.outs(w, footnoteRef(r.opts.FootnoteAnchorPrefix, nodeData))
		return
	}

	attrs = appendLinkAttrs(attrs, r.opts.Flags, dest)
	if len(nodeData.Title) > 0 {
		var titleBuff bytes.Buffer
		titleBuff.WriteString("title=\"")
		EscapeHTML(&titleBuff, nodeData.Title)
		titleBuff.WriteByte('"')
		attrs = append(attrs, titleBuff.String())
	}
	r.outTag(w, "<a", attrs)
}

func (r *Renderer) linkExit(w io.Writer, node *ast.Node, nodeData *ast.LinkData) {
	if nodeData.NoteID == 0 {
		r.outs(w, "</a>")
	}
}

func (r *Renderer) link(w io.Writer, node *ast.Node, nodeData *ast.LinkData, entering bool) {
	// mark it but don't link it if it is not a safe link: no smartypants
	if needSkipLink(r.opts.Flags, nodeData.Destination) {
		r.outOneOf(w, entering, "<tt>", "</tt>")
		return
	}

	if entering {
		r.linkEnter(w, node, nodeData)
	} else {
		r.linkExit(w, node, nodeData)
	}
}

func (r *Renderer) imageEnter(w io.Writer, node *ast.Node, nodeData *ast.ImageData) {
	dest := nodeData.Destination
	dest = r.addAbsPrefix(dest)
	if r.disableTags == 0 {
		//if options.safe && potentiallyUnsafe(dest) {
		//out(w, `<img src="" alt="`)
		//} else {
		r.outs(w, `<img src="`)
		escLink(w, dest)
		r.outs(w, `" alt="`)
		//}
	}
	r.disableTags++
}

func (r *Renderer) imageExit(w io.Writer, node *ast.Node, nodeData *ast.ImageData) {
	r.disableTags--
	if r.disableTags == 0 {
		if nodeData.Title != nil {
			r.outs(w, `" title="`)
			EscapeHTML(w, nodeData.Title)
		}
		r.outs(w, `" />`)
	}
}

func (r *Renderer) paragraphEnter(w io.Writer, node *ast.Node, nodeData *ast.ParagraphData) {
	// TODO: untangle this clusterfuck about when the newlines need
	// to be added and when not.
	prev := node.Prev()
	if prev != nil {
		switch prev.Data.(type) {
		case *ast.HTMLBlockData, *ast.ListData, *ast.ParagraphData, *ast.HeadingData, *ast.CodeBlockData, *ast.BlockQuoteData, *ast.HorizontalRuleData:
			r.cr(w)
		}
	}
	if isBlockQuoteData(node.Parent.Data) && prev == nil {
		r.cr(w)
	}
	r.outs(w, "<p>")
}

func (r *Renderer) paragraphExit(w io.Writer, node *ast.Node, nodeData *ast.ParagraphData) {
	r.outs(w, "</p>")
	if !(isListItemData(node.Parent.Data) && node.Next() == nil) {
		r.cr(w)
	}
}

func (r *Renderer) paragraph(w io.Writer, node *ast.Node, nodeData *ast.ParagraphData, entering bool) {
	if skipParagraphTags(node) {
		return
	}
	if entering {
		r.paragraphEnter(w, node, nodeData)
	} else {
		r.paragraphExit(w, node, nodeData)
	}
}
func (r *Renderer) image(w io.Writer, node *ast.Node, nodeData *ast.ImageData, entering bool) {
	if entering {
		r.imageEnter(w, node, nodeData)
	} else {
		r.imageExit(w, node, nodeData)
	}
}

func (r *Renderer) code(w io.Writer, node *ast.Node, nodeData *ast.CodeData) {
	r.outs(w, "<code>")
	EscapeHTML(w, node.Literal)
	r.outs(w, "</code>")
}

func (r *Renderer) htmlBlock(w io.Writer, node *ast.Node, nodeData *ast.HTMLBlockData) {
	if r.opts.Flags&SkipHTML != 0 {
		return
	}
	r.cr(w)
	r.out(w, node.Literal)
	r.cr(w)
}

func (r *Renderer) headingEnter(w io.Writer, node *ast.Node, nodeData *ast.HeadingData) {
	var attrs []string
	if nodeData.IsTitleblock {
		attrs = append(attrs, `class="title"`)
	}
	if nodeData.HeadingID != "" {
		id := r.ensureUniqueHeadingID(nodeData.HeadingID)
		if r.opts.HeadingIDPrefix != "" {
			id = r.opts.HeadingIDPrefix + id
		}
		if r.opts.HeadingIDSuffix != "" {
			id = id + r.opts.HeadingIDSuffix
		}
		attrID := `id="` + id + `"`
		attrs = append(attrs, attrID)
	}
	r.cr(w)
	r.outTag(w, headingOpenTagFromLevel(nodeData.Level), attrs)
}

func (r *Renderer) headingExit(w io.Writer, node *ast.Node, nodeData *ast.HeadingData) {
	r.outs(w, headingCloseTagFromLevel(nodeData.Level))
	if !(isListItemData(node.Parent.Data) && node.Next() == nil) {
		r.cr(w)
	}
}

func (r *Renderer) heading(w io.Writer, node *ast.Node, nodeData *ast.HeadingData, entering bool) {
	if entering {
		r.headingEnter(w, node, nodeData)
	} else {
		r.headingExit(w, node, nodeData)
	}
}

func (r *Renderer) horizontalRule(w io.Writer) {
	r.cr(w)
	r.outHRTag(w)
	r.cr(w)
}

func (r *Renderer) listEnter(w io.Writer, node *ast.Node, nodeData *ast.ListData) {
	// TODO: attrs don't seem to be set
	var attrs []string

	if nodeData.IsFootnotesList {
		r.outs(w, "\n<div class=\"footnotes\">\n\n")
		r.outHRTag(w)
		r.cr(w)
	}
	r.cr(w)
	if isListItemData(node.Parent.Data) {
		grand := node.Parent.Parent
		if isListTight(grand.Data) {
			r.cr(w)
		}
	}

	openTag := "<ul"
	if nodeData.ListFlags&ast.ListTypeOrdered != 0 {
		openTag = "<ol"
	}
	if nodeData.ListFlags&ast.ListTypeDefinition != 0 {
		openTag = "<dl"
	}
	r.outTag(w, openTag, attrs)
	r.cr(w)
}

func (r *Renderer) listExit(w io.Writer, node *ast.Node, nodeData *ast.ListData) {
	closeTag := "</ul>"
	if nodeData.ListFlags&ast.ListTypeOrdered != 0 {
		closeTag = "</ol>"
	}
	if nodeData.ListFlags&ast.ListTypeDefinition != 0 {
		closeTag = "</dl>"
	}
	r.outs(w, closeTag)

	//cr(w)
	//if node.parent.Type != Item {
	//	cr(w)
	//}
	if isListItemData(node.Parent.Data) && node.Next() != nil {
		r.cr(w)
	}
	if isDocumentData(node.Parent.Data) || isBlockQuoteData(node.Parent.Data) {
		r.cr(w)
	}
	if nodeData.IsFootnotesList {
		r.outs(w, "\n</div>\n")
	}
}

func (r *Renderer) list(w io.Writer, node *ast.Node, nodeData *ast.ListData, entering bool) {
	if entering {
		r.listEnter(w, node, nodeData)
	} else {
		r.listExit(w, node, nodeData)
	}
}

func (r *Renderer) listItemEnter(w io.Writer, node *ast.Node, nodeData *ast.ListItemData) {
	if itemOpenCR(node) {
		r.cr(w)
	}
	if nodeData.RefLink != nil {
		slug := slugify(nodeData.RefLink)
		r.outs(w, footnoteItem(r.opts.FootnoteAnchorPrefix, slug))
		return
	}

	openTag := "<li>"
	if nodeData.ListFlags&ast.ListTypeDefinition != 0 {
		openTag = "<dd>"
	}
	if nodeData.ListFlags&ast.ListTypeTerm != 0 {
		openTag = "<dt>"
	}
	r.outs(w, openTag)
}

func (r *Renderer) listItemExit(w io.Writer, node *ast.Node, nodeData *ast.ListItemData) {
	if nodeData.RefLink != nil && r.opts.Flags&FootnoteReturnLinks != 0 {
		slug := slugify(nodeData.RefLink)
		prefix := r.opts.FootnoteAnchorPrefix
		link := r.opts.FootnoteReturnLinkContents
		s := footnoteReturnLink(prefix, link, slug)
		r.outs(w, s)
	}

	closeTag := "</li>"
	if nodeData.ListFlags&ast.ListTypeDefinition != 0 {
		closeTag = "</dd>"
	}
	if nodeData.ListFlags&ast.ListTypeTerm != 0 {
		closeTag = "</dt>"
	}
	r.outs(w, closeTag)
	r.cr(w)
}

func (r *Renderer) listItem(w io.Writer, node *ast.Node, nodeData *ast.ListItemData, entering bool) {
	if entering {
		r.listItemEnter(w, node, nodeData)
	} else {
		r.listItemExit(w, node, nodeData)
	}
}

func (r *Renderer) codeBlock(w io.Writer, node *ast.Node, nodeData *ast.CodeBlockData) {
	var attrs []string
	attrs = appendLanguageAttr(attrs, nodeData.Info)
	r.cr(w)
	r.outs(w, "<pre>")
	r.outTag(w, "<code", attrs)
	EscapeHTML(w, node.Literal)
	r.outs(w, "</code>")
	r.outs(w, "</pre>")
	if !isListItemData(node.Parent.Data) {
		r.cr(w)
	}
}

func (r *Renderer) tableCell(w io.Writer, node *ast.Node, nodeData *ast.TableCellData, entering bool) {
	if !entering {
		r.outOneOf(w, nodeData.IsHeader, "</th>", "</td>")
		r.cr(w)
		return
	}

	// entering
	var attrs []string
	openTag := "<td"
	if nodeData.IsHeader {
		openTag = "<th"
	}
	align := cellAlignment(nodeData.Align)
	if align != "" {
		attrs = append(attrs, fmt.Sprintf(`align="%s"`, align))
	}
	if node.Prev() == nil {
		r.cr(w)
	}
	r.outTag(w, openTag, attrs)
}

func (r *Renderer) tableBody(w io.Writer, node *ast.Node, nodeData *ast.TableBodyData, entering bool) {
	if entering {
		r.cr(w)
		r.outs(w, "<tbody>")
		// XXX: this is to adhere to a rather silly test. Should fix test.
		if node.FirstChild() == nil {
			r.cr(w)
		}
	} else {
		r.outs(w, "</tbody>")
		r.cr(w)
	}
}

// RenderNode is a default renderer of a single node of a syntax tree. For
// block nodes it will be called twice: first time with entering=true, second
// time with entering=false, so that it could know when it's working on an open
// tag and when on close. It writes the result to w.
//
// The return value is a way to tell the calling walker to adjust its walk
// pattern: e.g. it can terminate the traversal by returning Terminate. Or it
// can ask the walker to skip a subtree of this node by returning SkipChildren.
// The typical behavior is to return GoToNext, which asks for the usual
// traversal to the next node.
func (r *Renderer) RenderNode(w io.Writer, node *ast.Node, entering bool) ast.WalkStatus {
	if r.opts.RenderNodeHook != nil {
		status, didHandle := r.opts.RenderNodeHook(w, node, entering)
		if didHandle {
			return status
		}
	}
	switch nodeData := node.Data.(type) {
	case *ast.TextData:
		r.text(w, node, nodeData)
	case *ast.SoftbreakData:
		r.cr(w)
		// TODO: make it configurable via out(renderer.softbreak)
	case *ast.HardbreakData:
		r.hardBreak(w, node, nodeData)
	case *ast.EmphData:
		r.outOneOf(w, entering, "<em>", "</em>")
	case *ast.StrongData:
		r.outOneOf(w, entering, "<strong>", "</strong>")
	case *ast.DelData:
		r.outOneOf(w, entering, "<del>", "</del>")
	case *ast.BlockQuoteData:
		r.outOneOfCr(w, entering, "<blockquote>", "</blockquote>")
	case *ast.LinkData:
		r.link(w, node, nodeData, entering)
	case *ast.ImageData:
		if r.opts.Flags&SkipImages != 0 {
			return ast.SkipChildren
		}
		r.image(w, node, nodeData, entering)
	case *ast.CodeData:
		r.code(w, node, nodeData)
	case *ast.CodeBlockData:
		r.codeBlock(w, node, nodeData)
	case *ast.DocumentData:
		// do nothing
	case *ast.ParagraphData:
		r.paragraph(w, node, nodeData, entering)
	case *ast.HTMLSpanData:
		r.span(w, node, nodeData)
	case *ast.HTMLBlockData:
		r.htmlBlock(w, node, nodeData)
	case *ast.HeadingData:
		r.heading(w, node, nodeData, entering)
	case *ast.HorizontalRuleData:
		r.horizontalRule(w)
	case *ast.ListData:
		r.list(w, node, nodeData, entering)
	case *ast.ListItemData:
		r.listItem(w, node, nodeData, entering)
	case *ast.TableData:
		r.outOneOfCr(w, entering, "<table>", "</table>")
	case *ast.TableCellData:
		r.tableCell(w, node, nodeData, entering)
	case *ast.TableHeadData:
		r.outOneOfCr(w, entering, "<thead>", "</thead>")
	case *ast.TableBodyData:
		r.tableBody(w, node, nodeData, entering)
	case *ast.TableRowData:
		r.outOneOfCr(w, entering, "<tr>", "</tr>")
	default:
		//panic("Unknown node type " + node.Type.String())
		panic(fmt.Sprintf("Unknown node type %T", node.Data))
	}
	return ast.GoToNext
}

// RenderHeader writes HTML document preamble and TOC if requested.
func (r *Renderer) RenderHeader(w io.Writer, ast *ast.Node) {
	r.writeDocumentHeader(w)
	if r.opts.Flags&TOC != 0 {
		r.writeTOC(w, ast)
	}
}

// RenderFooter writes HTML document footer.
func (r *Renderer) RenderFooter(w io.Writer, ast *ast.Node) {
	if r.opts.Flags&CompletePage == 0 {
		return
	}
	io.WriteString(w, "\n</body>\n</html>\n")
}

func (r *Renderer) writeDocumentHeader(w io.Writer) {
	if r.opts.Flags&CompletePage == 0 {
		return
	}
	ending := ""
	if r.opts.Flags&UseXHTML != 0 {
		io.WriteString(w, "<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML 1.0 Transitional//EN\" ")
		io.WriteString(w, "\"http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd\">\n")
		io.WriteString(w, "<html xmlns=\"http://www.w3.org/1999/xhtml\">\n")
		ending = " /"
	} else {
		io.WriteString(w, "<!DOCTYPE html>\n")
		io.WriteString(w, "<html>\n")
	}
	io.WriteString(w, "<head>\n")
	io.WriteString(w, "  <title>")
	if r.opts.Flags&Smartypants != 0 {
		r.sr.Process(w, []byte(r.opts.Title))
	} else {
		EscapeHTML(w, []byte(r.opts.Title))
	}
	io.WriteString(w, "</title>\n")
	io.WriteString(w, "  <meta name=\"GENERATOR\" content=\"github.com/gomarkdown/markdown markdown processor for Go")
	io.WriteString(w, "\"")
	io.WriteString(w, ending)
	io.WriteString(w, ">\n")
	io.WriteString(w, "  <meta charset=\"utf-8\"")
	io.WriteString(w, ending)
	io.WriteString(w, ">\n")
	if r.opts.CSS != "" {
		io.WriteString(w, "  <link rel=\"stylesheet\" type=\"text/css\" href=\"")
		EscapeHTML(w, []byte(r.opts.CSS))
		io.WriteString(w, "\"")
		io.WriteString(w, ending)
		io.WriteString(w, ">\n")
	}
	if r.opts.Icon != "" {
		io.WriteString(w, "  <link rel=\"icon\" type=\"image/x-icon\" href=\"")
		EscapeHTML(w, []byte(r.opts.Icon))
		io.WriteString(w, "\"")
		io.WriteString(w, ending)
		io.WriteString(w, ">\n")
	}
	io.WriteString(w, "</head>\n")
	io.WriteString(w, "<body>\n\n")
}

func (r *Renderer) writeTOC(w io.Writer, doc *ast.Node) {
	buf := bytes.Buffer{}

	inHeading := false
	tocLevel := 0
	headingCount := 0

	doc.WalkFunc(func(node *ast.Node, entering bool) ast.WalkStatus {
		if nodeData, ok := node.Data.(*ast.HeadingData); ok && !nodeData.IsTitleblock {
			inHeading = entering
			if entering {
				nodeData.HeadingID = fmt.Sprintf("toc_%d", headingCount)
				if nodeData.Level == tocLevel {
					buf.WriteString("</li>\n\n<li>")
				} else if nodeData.Level < tocLevel {
					for nodeData.Level < tocLevel {
						tocLevel--
						buf.WriteString("</li>\n</ul>")
					}
					buf.WriteString("</li>\n\n<li>")
				} else {
					for nodeData.Level > tocLevel {
						tocLevel++
						buf.WriteString("\n<ul>\n<li>")
					}
				}

				fmt.Fprintf(&buf, `<a href="#toc_%d">`, headingCount)
				headingCount++
			} else {
				buf.WriteString("</a>")
			}
			return ast.GoToNext
		}

		if inHeading {
			return r.RenderNode(&buf, node, entering)
		}

		return ast.GoToNext
	})

	for ; tocLevel > 0; tocLevel-- {
		buf.WriteString("</li>\n</ul>")
	}

	if buf.Len() > 0 {
		io.WriteString(w, "<nav>\n")
		w.Write(buf.Bytes())
		io.WriteString(w, "\n\n</nav>\n")
	}
	r.lastOutputLen = buf.Len()
}

func isListData(d ast.NodeData) bool {
	_, ok := d.(*ast.ListData)
	return ok
}

func isListTight(d ast.NodeData) bool {
	if listData, ok := d.(*ast.ListData); ok {
		return listData.Tight
	}
	return false
}

func isListItemData(d ast.NodeData) bool {
	_, ok := d.(*ast.ListItemData)
	return ok
}

func isListItemTerm(node *ast.Node) bool {
	data, ok := node.Data.(*ast.ListItemData)
	return ok && data.ListFlags&ast.ListTypeTerm != 0
}

func isLinkData(d ast.NodeData) bool {
	_, ok := d.(*ast.LinkData)
	return ok
}

func isBlockQuoteData(d ast.NodeData) bool {
	_, ok := d.(*ast.BlockQuoteData)
	return ok
}

func isDocumentData(d ast.NodeData) bool {
	_, ok := d.(*ast.DocumentData)
	return ok
}

// TODO: move to internal package
func skipSpace(data []byte, i int) int {
	n := len(data)
	for i < n && isSpace(data[i]) {
		i++
	}
	return i
}

// TODO: move to internal package
var validUris = [][]byte{[]byte("http://"), []byte("https://"), []byte("ftp://"), []byte("mailto://")}
var validPaths = [][]byte{[]byte("/"), []byte("./"), []byte("../")}

func isSafeLink(link []byte) bool {
	for _, path := range validPaths {
		if len(link) >= len(path) && bytes.Equal(link[:len(path)], path) {
			if len(link) == len(path) {
				return true
			} else if isAlnum(link[len(path)]) {
				return true
			}
		}
	}

	for _, prefix := range validUris {
		// TODO: handle unicode here
		// case-insensitive prefix test
		if len(link) > len(prefix) && bytes.Equal(bytes.ToLower(link[:len(prefix)]), prefix) && isAlnum(link[len(prefix)]) {
			return true
		}
	}

	return false
}

// TODO: move to internal package
// Create a url-safe slug for fragments
func slugify(in []byte) []byte {
	if len(in) == 0 {
		return in
	}
	out := make([]byte, 0, len(in))
	sym := false

	for _, ch := range in {
		if isAlnum(ch) {
			sym = false
			out = append(out, ch)
		} else if sym {
			continue
		} else {
			out = append(out, '-')
			sym = true
		}
	}
	var a, b int
	var ch byte
	for a, ch = range out {
		if ch != '-' {
			break
		}
	}
	for b = len(out) - 1; b > 0; b-- {
		if out[b] != '-' {
			break
		}
	}
	return out[a : b+1]
}

// TODO: move to internal package
// isAlnum returns true if c is a digit or letter
// TODO: check when this is looking for ASCII alnum and when it should use unicode
func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || isLetter(c)
}

// isSpace returns true if c is a white-space charactr
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// isLetter returns true if c is ascii letter
func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isPunctuation returns true if c is a punctuation symbol.
func isPunctuation(c byte) bool {
	for _, r := range []byte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~") {
		if c == r {
			return true
		}
	}
	return false
}
