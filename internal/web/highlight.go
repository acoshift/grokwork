package web

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token classes are a closed set. The template copies Class into a CSS
// class, so a value never comes from the file.
const (
	tokKW  = "kw"
	tokStr = "str"
	tokCom = "com"
	tokNum = "num"
	tokTyp = "typ"
)

// codeToken is one run of source text. An empty Class is plain text.
// Text is raw source. The template escapes it; nothing here is HTML.
type codeToken struct {
	Class string
	Text  string
}

type lang struct {
	keywords map[string]struct{}
	line     []string
	block    bool
	quotes   string
	triple   bool
	types    bool
	foldKW   bool
}

type lexState struct {
	closer string
}

// highlightSource colors body by the file name's extension.
// An unknown extension is plain text. An empty body is no lines.
// More than codeMaxLines returns no lines: the caller shows the truncation
// notice and does not paint a partial file.
func highlightSource(filename, body string) []codeLine {
	parts, tooMany := splitCodeLines(body)
	if tooMany || parts == nil {
		return nil
	}
	return highlightParts(filename, parts)
}

func highlightParts(filename string, parts []string) []codeLine {
	lines := make([]codeLine, len(parts))
	lg, ok := langFor(filename)
	if !ok {
		for i, t := range parts {
			lines[i] = codeLine{N: i + 1, Tokens: plainToken(t)}
		}
		return lines
	}
	var st lexState
	for i, t := range parts {
		lines[i] = codeLine{N: i + 1, Tokens: lexLine(lg, t, &st)}
	}
	return lines
}

func plainCodeLines(parts []string) []codeLine {
	lines := make([]codeLine, len(parts))
	for i, t := range parts {
		lines[i] = codeLine{N: i + 1, Tokens: plainToken(t)}
	}
	return lines
}

func plainToken(t string) []codeToken {
	if t == "" {
		return nil
	}
	return []codeToken{{Text: t}}
}

func langFor(filename string) (lang, bool) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".go":
		return lang{keywords: goKW, line: []string{"//"}, block: true, quotes: "`\"'", types: true}, true
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		return lang{keywords: jsKW, line: []string{"//"}, block: true, quotes: "`\"'"}, true
	case ".java", ".kt", ".kts", ".cs", ".scala", ".swift":
		return lang{keywords: cLikeKW, line: []string{"//"}, block: true, quotes: "\"'"}, true
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh":
		return lang{keywords: cKW, line: []string{"//"}, block: true, quotes: "\"'"}, true
	case ".rs":
		return lang{keywords: rustKW, line: []string{"//"}, block: true, quotes: "\"'"}, true
	case ".py", ".pyw":
		return lang{keywords: pyKW, line: []string{"#"}, quotes: "\"'", triple: true}, true
	case ".rb":
		return lang{keywords: rubyKW, line: []string{"#"}, quotes: "\"'"}, true
	case ".php":
		return lang{keywords: phpKW, line: []string{"//", "#"}, block: true, quotes: "\"'"}, true
	case ".sh", ".bash", ".zsh":
		return lang{keywords: shKW, line: []string{"#"}, quotes: "\"'"}, true
	case ".css", ".scss":
		return lang{block: true, quotes: "\"'"}, true
	case ".json":
		return lang{keywords: jsonKW, quotes: "\""}, true
	case ".yaml", ".yml", ".toml":
		return lang{line: []string{"#"}, quotes: "\"'"}, true
	case ".sql":
		return lang{keywords: sqlKW, line: []string{"--"}, block: true, quotes: "\"'", foldKW: true}, true
	case ".lua":
		return lang{keywords: luaKW, line: []string{"--"}, quotes: "\"'"}, true
	case ".mod":
		return lang{keywords: modKW, line: []string{"//"}, quotes: "`\""}, true
	default:
		return lang{}, false
	}
}

func lexLine(lg lang, line string, st *lexState) []codeToken {
	var toks []codeToken
	flush := func(class, text string) {
		if text == "" {
			return
		}
		if n := len(toks); n > 0 && toks[n-1].Class == class {
			toks[n-1].Text += text
			return
		}
		toks = append(toks, codeToken{Class: class, Text: text})
	}
	i := 0
	if st.closer != "" {
		class := tokStr
		if st.closer == "*/" {
			class = tokCom
		}
		if j := strings.Index(line, st.closer); j >= 0 {
			flush(class, line[:j+len(st.closer)])
			i = j + len(st.closer)
			st.closer = ""
		} else {
			flush(class, line)
			return toks
		}
	}
	for i < len(line) {
		rest := line[i:]
		r, size := utf8.DecodeRuneInString(rest)
		if lg.block && strings.HasPrefix(rest, "/*") {
			if j := strings.Index(rest, "*/"); j >= 0 {
				flush(tokCom, rest[:j+2])
				i += j + 2
				continue
			}
			flush(tokCom, rest)
			st.closer = "*/"
			break
		}
		if lg.triple && (strings.HasPrefix(rest, `"""`) || strings.HasPrefix(rest, "'''")) {
			closer := rest[:3]
			if j := strings.Index(rest[3:], closer); j >= 0 {
				flush(tokStr, rest[:3+j+3])
				i += 3 + j + 3
				continue
			}
			flush(tokStr, rest)
			st.closer = closer
			break
		}
		if strings.ContainsRune(lg.quotes, r) {
			end, closed := scanQuoted(line, i, byte(r))
			flush(tokStr, line[i:end])
			i = end
			if !closed && r == '`' {
				st.closer = "`"
			}
			continue
		}
		if p, ok := lineComment(lg, rest); ok {
			flush(tokCom, rest)
			i += len(p)
			break
		}
		if isIdentStart(r) {
			j := i + size
			for j < len(line) {
				rr, sz := utf8.DecodeRuneInString(line[j:])
				if !isIdentCont(rr) {
					break
				}
				j += sz
			}
			word := line[i:j]
			flush(identClass(lg, word, r), word)
			i = j
			continue
		}
		if r >= '0' && r <= '9' {
			j := scanNumber(line, i)
			flush(tokNum, line[i:j])
			i = j
			continue
		}
		flush("", line[i:i+size])
		i += size
	}
	return toks
}

func lineComment(lg lang, rest string) (string, bool) {
	for _, p := range lg.line {
		if strings.HasPrefix(rest, p) {
			return p, true
		}
	}
	return "", false
}

func identClass(lg lang, word string, first rune) string {
	key := word
	if lg.foldKW {
		key = strings.ToLower(word)
	}
	if _, ok := lg.keywords[key]; ok {
		return tokKW
	}
	if lg.types && unicode.IsUpper(first) {
		return tokTyp
	}
	return ""
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentCont(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func scanQuoted(s string, i int, quote byte) (int, bool) {
	j := i + 1
	for j < len(s) {
		if quote != '`' && s[j] == '\\' {
			j += 2
			continue
		}
		if s[j] == quote {
			return j + 1, true
		}
		j++
	}
	if j > len(s) {
		return len(s), false
	}
	return j, false
}

func scanNumber(s string, i int) int {
	j := i
	if j+1 < len(s) && s[j] == '0' {
		switch s[j+1] {
		case 'x', 'X', 'b', 'B', 'o', 'O':
			j += 2
			for j < len(s) {
				c := s[j]
				if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '_' {
					j++
					continue
				}
				break
			}
			return j
		}
	}
	dot := false
	for j < len(s) {
		c := s[j]
		switch {
		case c >= '0' && c <= '9' || c == '_':
			j++
		case c == '.' && !dot:
			dot = true
			j++
		default:
			return j
		}
	}
	return j
}

func wordSet(list string) map[string]struct{} {
	m := make(map[string]struct{})
	for w := range strings.FieldsSeq(list) {
		m[w] = struct{}{}
	}
	return m
}

var (
	goKW    = wordSet(`break default func interface select case defer go map struct chan else goto package switch const fallthrough if range type continue for import return var`)
	jsKW    = wordSet(`break case catch class const continue debugger default delete do else export extends finally for function if import in instanceof let new return static super switch this throw try typeof var void while with yield async await of`)
	cLikeKW = wordSet(`break case catch class const continue default do else enum extends final finally for goto if implements import instanceof interface native new package private protected public return static super switch this throw throws try var void while true false null`)
	cKW     = wordSet(`auto break case char const continue default do double else enum extern float for goto if inline int long register restrict return short signed sizeof static struct switch typedef union unsigned void volatile while _Bool _Complex _Imaginary`)
	rustKW  = wordSet(`as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while`)
	pyKW    = wordSet(`False None True and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield`)
	rubyKW  = wordSet(`BEGIN END alias and begin break case class def defined? do else elsif end ensure false for if in module next nil not or redo rescue retry return self super then true undef unless until when while yield`)
	phpKW   = wordSet(`abstract and array as break case catch class clone const continue declare default do echo else elseif empty enddeclare endfor endforeach endif endswitch endwhile extends final finally fn for foreach function global goto if implements include include_once instanceof insteadof interface isset list match namespace new or print private protected public require require_once return static switch throw trait try unset use var while yield`)
	shKW    = wordSet(`if then else elif fi case esac for while until do done in function select time coproc`)
	jsonKW  = wordSet(`true false null`)
	sqlKW   = wordSet(`select from where and or not insert into update set delete create table drop alter join left right inner outer on as group by order having limit offset values distinct union all is null in like between exists primary key foreign references index view with case when then else end begin commit rollback`)
	luaKW   = wordSet(`and break do else elseif end false for function goto if in local nil not or repeat return then true until while`)
	modKW   = wordSet(`module go require replace exclude retract`)
)
