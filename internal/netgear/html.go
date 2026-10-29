package netgear

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// A tiny query layer over golang.org/x/net/html, enough for the NETGEAR
// pages: find elements by tag, attribute and class, read attributes and
// text. Every lookup returns zero values when markup is missing, so a
// firmware change degrades to "unknown" rather than a panic.

type node struct{ *html.Node }

type match func(node) bool

func parseHTML(page []byte) node {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil { // x/net/html recovers from any input; kept for safety
		return node{&html.Node{Type: html.DocumentNode}}
	}
	return node{doc}
}

func (n node) attr(key string) (string, bool) {
	if n.Node == nil {
		return "", false
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func (n node) val(key string) string {
	v, _ := n.attr(key)
	return v
}

// text returns the concatenated text of n and its descendants.
func (n node) text() string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	if n.Node != nil {
		walk(n.Node)
	}
	return b.String()
}

// all returns every descendant element matching all conditions, in
// document order.
func (n node) all(conds ...match) []node {
	var out []node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && matches(node{c}, conds) {
				out = append(out, node{c})
			}
			walk(c)
		}
	}
	if n.Node != nil {
		walk(n.Node)
	}
	return out
}

// first returns the first descendant matching all conditions, or a zero node.
func (n node) first(conds ...match) node {
	if found := n.all(conds...); len(found) > 0 {
		return found[0]
	}
	return node{}
}

func (n node) ok() bool { return n.Node != nil }

// children returns the direct child elements matching all conditions.
func (n node) children(conds ...match) []node {
	var out []node
	if n.Node == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && matches(node{c}, conds) {
			out = append(out, node{c})
		}
	}
	return out
}

// nextSiblings returns the following sibling elements matching all
// conditions.
func (n node) nextSiblings(conds ...match) []node {
	var out []node
	if n.Node == nil {
		return nil
	}
	for c := n.NextSibling; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && matches(node{c}, conds) {
			out = append(out, node{c})
		}
	}
	return out
}

func matches(n node, conds []match) bool {
	for _, c := range conds {
		if !c(n) {
			return false
		}
	}
	return true
}

func tag(name string) match { return func(n node) bool { return n.Data == name } }

func attrIs(key, value string) match {
	return func(n node) bool { v, ok := n.attr(key); return ok && v == value }
}

func hasAttr(key string) match { return func(n node) bool { _, ok := n.attr(key); return ok } }

// classContains matches a substring of the class attribute (XPath
// contains(@class, ...)).
func classContains(s string) match {
	return func(n node) bool { return strings.Contains(n.val("class"), s) }
}
