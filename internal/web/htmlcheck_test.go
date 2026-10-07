package web

import (
	"fmt"
	"regexp"
	"strings"
)

// checkMarkup strictly checks rendered markup: well-formed and nested, and
// nothing the CSP refuses (event handlers, inline styles, other scripts).

var (
	tagNameRE  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	attrNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*$`)
)

// voidElements never have an end tag.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

// urlAttrs hold addresses, which must never be javascript: ones.
var urlAttrs = map[string]bool{"href": true, "src": true, "action": true, "formaction": true}

// markupTag is one start tag found by checkMarkup.
type markupTag struct {
	Name  string
	Attrs map[string]string
}

// checkMarkup returns the start tags of html in order, or why it isn't
// markup the templates may produce.
func checkMarkup(html string) ([]markupTag, error) {
	var tags []markupTag
	var stack []string
	i := 0
	errAt := func(format string, args ...any) error {
		lo, hi := max(i-60, 0), min(i+60, len(html))
		return fmt.Errorf("at byte %d (…%s…): %s", i, html[lo:hi], fmt.Sprintf(format, args...))
	}
	for i < len(html) {
		lt := strings.IndexByte(html[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		rest := html[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest, "-->")
			if end < 0 {
				return nil, errAt("unclosed comment")
			}
			i += end + 3
			continue
		case strings.HasPrefix(rest, "<!doctype html>"):
			i += len("<!doctype html>")
			continue
		case strings.HasPrefix(rest, "</"):
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return nil, errAt("unclosed end tag")
			}
			name := rest[2:end]
			if !tagNameRE.MatchString(name) {
				return nil, errAt("bad end tag %q", name)
			}
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return nil, errAt("end tag </%s> closes %v", name, stack)
			}
			stack = stack[:len(stack)-1]
			i += end + 1
			continue
		}
		j := 1
		for j < len(rest) && (rest[j] >= 'a' && rest[j] <= 'z' || rest[j] >= '0' && rest[j] <= '9' || rest[j] == '-') {
			j++
		}
		name := rest[1:j]
		if !tagNameRE.MatchString(name) {
			return nil, errAt("a bare '<' in text: unescaped markup")
		}
		tag := markupTag{Name: name, Attrs: map[string]string{}}
		selfClosing := false
		for {
			for j < len(rest) && strings.IndexByte(" \t\n\r\f", rest[j]) >= 0 {
				j++
			}
			if j >= len(rest) {
				return nil, errAt("unclosed start tag <%s", name)
			}
			if rest[j] == '>' {
				j++
				break
			}
			if strings.HasPrefix(rest[j:], "/>") {
				selfClosing = true
				j += 2
				break
			}
			k := j
			for k < len(rest) && strings.IndexByte(" \t\n\r\f/>=\"'", rest[k]) < 0 {
				k++
			}
			attr := rest[j:k]
			if !attrNameRE.MatchString(attr) {
				return nil, errAt("bad attribute name %q in <%s>", attr, name)
			}
			if _, dup := tag.Attrs[attr]; dup {
				return nil, errAt("attribute %s twice in <%s>", attr, name)
			}
			if strings.HasPrefix(strings.ToLower(attr), "on") || strings.EqualFold(attr, "style") {
				return nil, errAt("attribute %s in <%s>: the CSP forbids inline handlers and styles", attr, name)
			}
			j = k
			value := ""
			if j < len(rest) && rest[j] == '=' {
				j++
				if j >= len(rest) || rest[j] != '"' {
					return nil, errAt("unquoted value of %s in <%s>", attr, name)
				}
				end := strings.IndexByte(rest[j+1:], '"')
				if end < 0 {
					return nil, errAt("unclosed value of %s in <%s>", attr, name)
				}
				value = rest[j+1 : j+1+end]
				if strings.ContainsAny(value, "<>") {
					return nil, errAt("unescaped < or > in the value of %s in <%s>", attr, name)
				}
				j += end + 2
			}
			if urlAttrs[attr] && strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "javascript:") {
				return nil, errAt("javascript: URL in %s of <%s>", attr, name)
			}
			tag.Attrs[attr] = value
		}
		i += j
		tags = append(tags, tag)
		if name == "script" {
			if !strings.HasPrefix(tag.Attrs["src"], "/static/app.js?") || !strings.HasPrefix(html[i:], "</script>") {
				return nil, errAt("a script other than the empty, external app.js")
			}
		}
		if name == "style" {
			return nil, errAt("an inline <style>")
		}
		if !selfClosing && !voidElements[name] {
			stack = append(stack, name)
		}
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("unclosed elements at the end: %v", stack)
	}
	return tags, nil
}
