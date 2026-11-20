package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Directive es una directiva de la configuración: `name arg1 arg2;`
// o un bloque `name arg1 { ... }`.
type Directive struct {
	Name     string
	Args     []string
	Block    []*Directive
	HasBlock bool
	Line     int
}

type token struct {
	s    string
	kind byte // 'w' palabra, '{', '}', ';'
	line int
}

func tokenize(src string) ([]token, error) {
	var toks []token
	line, i := 1, 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == ';':
			toks = append(toks, token{string(c), c, line})
			i++
		case c == '"' || c == '\'':
			q := c
			start := line
			i++
			var sb strings.Builder
			closed := false
			for i < len(src) {
				ch := src[i]
				if ch == '\\' && i+1 < len(src) && (src[i+1] == q || src[i+1] == '\\') {
					sb.WriteByte(src[i+1])
					i += 2
					continue
				}
				if ch == q {
					i++
					closed = true
					break
				}
				if ch == '\n' {
					line++
				}
				sb.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("línea %d: cadena sin cerrar", start)
			}
			toks = append(toks, token{sb.String(), 'w', start})
		default:
			start := i
			for i < len(src) && !strings.ContainsRune(" \t\r\n;{}", rune(src[i])) {
				i++
			}
			toks = append(toks, token{src[start:i], 'w', line})
		}
	}
	return toks, nil
}

func parseBlock(toks []token, pos *int, top bool) ([]*Directive, error) {
	var out []*Directive
	for *pos < len(toks) {
		t := toks[*pos]
		if t.kind == '}' {
			if top {
				return nil, fmt.Errorf("línea %d: '}' inesperado", t.line)
			}
			*pos++
			return out, nil
		}
		if t.kind != 'w' {
			return nil, fmt.Errorf("línea %d: token inesperado %q", t.line, t.s)
		}
		d := &Directive{Name: t.s, Line: t.line}
		*pos++
		for *pos < len(toks) && toks[*pos].kind == 'w' {
			d.Args = append(d.Args, toks[*pos].s)
			*pos++
		}
		if *pos >= len(toks) {
			return nil, fmt.Errorf("línea %d: fin de fichero inesperado tras %q", d.Line, d.Name)
		}
		switch toks[*pos].kind {
		case ';':
			*pos++
		case '{':
			*pos++
			d.HasBlock = true
			blk, err := parseBlock(toks, pos, false)
			if err != nil {
				return nil, err
			}
			d.Block = blk
		default:
			return nil, fmt.Errorf("línea %d: se esperaba ';' o '{' tras %q", toks[*pos].line, d.Name)
		}
		out = append(out, d)
	}
	if !top {
		return nil, fmt.Errorf("falta '}' al final del fichero")
	}
	return out, nil
}

// expandIncludes sustituye `include patrón;` por el contenido de los ficheros.
func expandIncludes(dirs []*Directive, base string, depth int) ([]*Directive, error) {
	if depth > 10 {
		return nil, fmt.Errorf("include demasiado anidado")
	}
	var out []*Directive
	for _, d := range dirs {
		if d.Name == "include" && len(d.Args) == 1 {
			pat := d.Args[0]
			if !filepath.IsAbs(pat) {
				pat = filepath.Join(base, pat)
			}
			files, err := filepath.Glob(pat)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				sub, err := parseFile(f, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, sub...)
			}
			continue
		}
		if d.HasBlock {
			blk, err := expandIncludes(d.Block, base, depth)
			if err != nil {
				return nil, err
			}
			d.Block = blk
		}
		out = append(out, d)
	}
	return out, nil
}

func parseFile(path string, depth int) ([]*Directive, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	toks, err := tokenize(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pos := 0
	dirs, err := parseBlock(toks, &pos, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return expandIncludes(dirs, filepath.Dir(path), depth)
}

// ParseConfig lee y parsea un fichero de configuración estilo nginx.
func ParseConfig(path string) ([]*Directive, error) { return parseFile(path, 0) }

// scope encadena directivas con su ámbito padre (location → server → http)
// para emular la herencia de nginx.
type scope struct {
	dirs   []*Directive
	parent *scope
}

func (s *scope) first(name string) *Directive {
	for c := s; c != nil; c = c.parent {
		for _, d := range c.dirs {
			if d.Name == name {
				return d
			}
		}
	}
	return nil
}

// all devuelve las directivas del ámbito más cercano que defina alguna.
func (s *scope) all(name string) []*Directive {
	for c := s; c != nil; c = c.parent {
		var out []*Directive
		for _, d := range c.dirs {
			if d.Name == name {
				out = append(out, d)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}
