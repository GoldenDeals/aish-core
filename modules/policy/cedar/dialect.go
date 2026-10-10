package cedar

import "strings"

const askReplacement = `@decision("ask") permit`

// rewriteAsk turns the effect "ask(" into `@decision("ask") permit(` without
// adding line breaks, so positions in Cedar errors match the source file.
// "ask" is an effect only outside parentheses and braces; strings, comments
// and annotation names are left alone.
func rewriteAsk(src string) string {
	var out strings.Builder
	depth := 0
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"':
			j := skipString(src, i)
			out.WriteString(src[i:j])
			i = j
		case c == '/' && strings.HasPrefix(src[i:], "//"):
			j := i + strings.IndexByte(src[i:]+"\n", '\n')
			out.WriteString(src[i:j])
			i = j
		case isIdent(c):
			j := i
			for j < len(src) && isIdent(src[j]) {
				j++
			}
			word := src[i:j]
			if depth == 0 && word == "ask" && (i == 0 || src[i-1] != '@') && strings.HasPrefix(strings.TrimLeft(src[j:], " \t\r\n"), "(") {
				word = askReplacement
			}
			out.WriteString(word)
			i = j
		default:
			switch c {
			case '(', '{', '[':
				depth++
			case ')', '}', ']':
				depth--
			}
			out.WriteByte(c)
			i++
		}
	}

	return out.String()
}

// skipString returns the index after the string literal that starts at i.
func skipString(src string, i int) int {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}

	return len(src)
}

func isIdent(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
