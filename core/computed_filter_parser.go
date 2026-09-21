package core

import (
	"fmt"
	"strconv"
	"strings"
)

// Minimal fexpr-compatible parser used for evaluating filters that reference
// computed (virtual) fields in-memory.
//
// It supports the subset of the fexpr grammar that is commonly used:
//
//	literal  := number | 'single quoted' | "double quoted" | true|false|null
//	ident    := [@a-zA-Z_][\w.\:]*   (eg. id, rel.name, @request.auth.id, a:each)
//	op       := = != ~ !~ > >= < <=
//	factor   := ident op literal | ident '[' literal ']' op literal | '(' expr ')'
//	term     := factor (&& factor)*
//	expr     := term (|| term)*
//
// This is intentionally NOT a full fexpr reimplementation - computed filters
// are post-processed in-memory and only simple direct/computed field comparisons
// are guaranteed to work. Relational access (".") inside computed post-filters
// is supported against already expanded records and the script-provided values.

type cfTokenKind int

const (
	cfTokEOF cfTokenKind = iota
	cfTokIdent
	cfTokString
	cfTokNumber
	cfTokOp
	cfTokLParen
	cfTokRParen
	cfTokLBracket
	cfTokRBracket
	cfTokAnd
	cfTokOr
	cfTokComma
)

type cfToken struct {
	kind cfTokenKind
	raw  string
	num  float64
}

type cfLexer struct {
	input string
	pos   int
}

func newCFLexer(input string) *cfLexer {
	return &cfLexer{input: input}
}

func (l *cfLexer) next() (cfToken, error) {
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
			continue
		case c == '(':
			l.pos++
			return cfToken{kind: cfTokLParen, raw: "("}, nil
		case c == ')':
			l.pos++
			return cfToken{kind: cfTokRParen, raw: ")"}, nil
		case c == '[':
			l.pos++
			return cfToken{kind: cfTokLBracket, raw: "["}, nil
		case c == ']':
			l.pos++
			return cfToken{kind: cfTokRBracket, raw: "]"}, nil
		case c == ',':
			l.pos++
			return cfToken{kind: cfTokComma, raw: ","}, nil
		case c == '\'' || c == '"':
			return l.readString(c)
		case c >= '0' && c <= '9' || (c == '-' && l.pos+1 < len(l.input) && l.input[l.pos+1] >= '0' && l.input[l.pos+1] <= '9'):
			return l.readNumber()
		case isIdentStart(c):
			return l.readIdent()
		case c == '&':
			if l.pos+1 < len(l.input) && l.input[l.pos+1] == '&' {
				l.pos += 2
				return cfToken{kind: cfTokAnd, raw: "&&"}, nil
			}
			return cfToken{}, fmt.Errorf("unexpected character %q at position %d", string(c), l.pos)
		case c == '|':
			// '||' is the OR operator; a single '|' is not part of the grammar
			// (fexpr uses '~' for LIKE)
			if l.pos+1 < len(l.input) && l.input[l.pos+1] == '|' {
				l.pos += 2
				return cfToken{kind: cfTokOr, raw: "||"}, nil
			}
			return cfToken{}, fmt.Errorf("unexpected character %q at position %d", string(c), l.pos)
		case c == '=' || c == '!' || c == '~' || c == '>' || c == '<':
			return l.readOp()
		default:
			return cfToken{}, fmt.Errorf("unexpected character %q at position %d", string(c), l.pos)
		}
	}
	return cfToken{kind: cfTokEOF}, nil
}

func isIdentStart(c byte) bool {
	return c == '@' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '.' || c == ':'
}

func (l *cfLexer) readString(quote byte) (cfToken, error) {
	l.pos++ // consume opening quote
	var sb strings.Builder
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if c == '\\' && l.pos+1 < len(l.input) {
			next := l.input[l.pos+1]
			switch next {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '\\':
				sb.WriteByte('\\')
			case '\'':
				sb.WriteByte('\'')
			case '"':
				sb.WriteByte('"')
			default:
				sb.WriteByte(next)
			}
			l.pos += 2
			continue
		}
		if c == quote {
			l.pos++
			return cfToken{kind: cfTokString, raw: sb.String()}, nil
		}
		sb.WriteByte(c)
		l.pos++
	}
	return cfToken{}, fmt.Errorf("unterminated string literal")
}

func (l *cfLexer) readNumber() (cfToken, error) {
	start := l.pos
	if l.input[l.pos] == '-' {
		l.pos++
	}
	hasDot := false
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if c >= '0' && c <= '9' {
			l.pos++
			continue
		}
		if c == '.' && !hasDot {
			hasDot = true
			l.pos++
			continue
		}
		break
	}
	raw := l.input[start:l.pos]
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return cfToken{}, fmt.Errorf("invalid number %q", raw)
	}
	return cfToken{kind: cfTokNumber, raw: raw, num: n}, nil
}

func (l *cfLexer) readIdent() (cfToken, error) {
	start := l.pos
	for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
		l.pos++
	}
	raw := l.input[start:l.pos]
	switch raw {
	case "&&", "and":
		return cfToken{kind: cfTokAnd, raw: raw}, nil
	case "||", "or":
		return cfToken{kind: cfTokOr, raw: raw}, nil
	case "true", "false":
		return cfToken{kind: cfTokIdent, raw: raw}, nil
	}
	return cfToken{kind: cfTokIdent, raw: raw}, nil
}

func (l *cfLexer) readOp() (cfToken, error) {
	start := l.pos
	c := l.input[l.pos]
	switch c {
	case '=':
		l.pos++
		return cfToken{kind: cfTokOp, raw: "="}, nil
	case '!':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return cfToken{kind: cfTokOp, raw: "!="}, nil
		}
		l.pos++
		return cfToken{}, fmt.Errorf("unexpected '!' at %d", start)
	case '~':
		l.pos++
		return cfToken{kind: cfTokOp, raw: "~"}, nil
	case '>':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return cfToken{kind: cfTokOp, raw: ">="}, nil
		}
		l.pos++
		return cfToken{kind: cfTokOp, raw: ">"}, nil
	case '<':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return cfToken{kind: cfTokOp, raw: "<="}, nil
		}
		l.pos++
		return cfToken{kind: cfTokOp, raw: "<"}, nil
	}
	return cfToken{}, fmt.Errorf("invalid operator at %d", start)
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

type cfNode interface{ cfNode() }

type cfBinary struct {
	op          string // "&&" or "||"
	left, right cfNode
}

type cfComparison struct {
	identifier string // eg. "label", "items:each" or "tags[123]"
	indexKey   string // optional bracket key (string/number)
	op         string
	value      cfValue
}

func (*cfBinary) cfNode()     {}
func (*cfComparison) cfNode() {}

type cfValue struct {
	kind   cfValueKind
	str    string
	num    float64
	boolv  bool
	isNull bool
}
type cfValueKind int

const (
	cfValString cfValueKind = iota
	cfValNumber
	cfValBool
	cfValNull
)

type cfParser struct {
	lexer *cfLexer
	cur   cfToken
}

func newCFParser(input string) *cfParser {
	p := &cfParser{lexer: newCFLexer(input)}
	return p
}

func (p *cfParser) advance() error {
	t, err := p.lexer.next()
	if err != nil {
		return err
	}
	p.cur = t
	return nil
}

// parse is the entry point.
func (p *cfParser) parse() (cfNode, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.cur.kind != cfTokEOF {
		return nil, fmt.Errorf("unexpected token %q", p.cur.raw)
	}
	return node, nil
}

// or := and ('||' and)*
func (p *cfParser) parseOr() (cfNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == cfTokOr {
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &cfBinary{op: "||", left: left, right: right}
	}
	return left, nil
}

// and := factor ('&&' factor)*
func (p *cfParser) parseAnd() (cfNode, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for p.cur.kind == cfTokAnd {
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		left = &cfBinary{op: "&&", left: left, right: right}
	}
	return left, nil
}

// factor := '(' or ')' | ident ('[' key ']')? op literal
func (p *cfParser) parseFactor() (cfNode, error) {
	if p.cur.kind == cfTokLParen {
		if err := p.advance(); err != nil {
			return nil, err
		}
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.cur.kind != cfTokRParen {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		return node, nil
	}

	if p.cur.kind != cfTokIdent {
		return nil, fmt.Errorf("expected field identifier, got %q", p.cur.raw)
	}
	ident := p.cur.raw

	if err := p.advance(); err != nil {
		return nil, err
	}

	// optional [key]
	indexKey := ""
	if p.cur.kind == cfTokLBracket {
		if err := p.advance(); err != nil {
			return nil, err
		}
		switch p.cur.kind {
		case cfTokString, cfTokNumber, cfTokIdent:
			indexKey = p.cur.raw
		default:
			return nil, fmt.Errorf("invalid subscript key %q", p.cur.raw)
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.cur.kind != cfTokRBracket {
			return nil, fmt.Errorf("missing ]")
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}

	if p.cur.kind != cfTokOp {
		return nil, fmt.Errorf("expected comparison operator after %q, got %q", ident, p.cur.raw)
	}
	op := p.cur.raw
	if err := p.advance(); err != nil {
		return nil, err
	}

	val, err := tokenToValue(p.cur)
	if err != nil {
		return nil, err
	}
	if err := p.advance(); err != nil {
		return nil, err
	}

	return &cfComparison{
		identifier: ident,
		indexKey:   indexKey,
		op:         op,
		value:      val,
	}, nil
}

func tokenToValue(t cfToken) (cfValue, error) {
	switch t.kind {
	case cfTokNumber:
		return cfValue{kind: cfValNumber, num: t.num}, nil
	case cfTokString:
		return cfValue{kind: cfValString, str: t.raw}, nil
	case cfTokIdent:
		switch t.raw {
		case "true":
			return cfValue{kind: cfValBool, boolv: true}, nil
		case "false":
			return cfValue{kind: cfValBool, boolv: false}, nil
		case "null":
			return cfValue{kind: cfValNull, isNull: true}, nil
		}
	}
	return cfValue{}, fmt.Errorf("expected literal value, got %q", t.raw)
}
