package css_parser

import (
	"fmt"
	"strings"

	"github.com/evanw/esbuild/internal/ast"
	"github.com/evanw/esbuild/internal/compat"
	"github.com/evanw/esbuild/internal/css_ast"
	"github.com/evanw/esbuild/internal/css_lexer"
	"github.com/evanw/esbuild/internal/logger"
)

// This is mostly a normal CSS parser with one exception: the addition of
// support for parsing https://drafts.csswg.org/css-nesting-1/.

type parser struct {
	log           logger.Log
	source        logger.Source
	tracker       logger.LineColumnTracker
	options       Options
	tokens        []css_lexer.Token
	stack         []css_lexer.T
	index         int
	end           int
	prevError     logger.Loc
	importRecords []ast.ImportRecord
}

type Options struct {
	UnsupportedCSSFeatures compat.CSSFeature
	MangleSyntax           bool
	RemoveWhitespace       bool
}

func Parse(log logger.Log, source logger.Source, options Options) css_ast.AST {
	p := parser{
		log:       log,
		source:    source,
		tracker:   logger.MakeLineColumnTracker(&source),
		options:   options,
		tokens:    css_lexer.Tokenize(log, source),
		prevError: logger.Loc{Start: -1},
	}
	p.end = len(p.tokens)
	tree := css_ast.AST{}
	tree.Rules = p.parseListOfRules(ruleContext{
		isTopLevel:     true,
		parseSelectors: true,
	})
	tree.ImportRecords = p.importRecords
	p.expect(css_lexer.TEndOfFile)
	return tree
}

func (p *parser) advance() {
	if p.index < p.end {
		p.index++
	}
}

func (p *parser) at(index int) css_lexer.Token {
	if index < p.end {
		return p.tokens[index]
	}
	if p.end < len(p.tokens) {
		return css_lexer.Token{
			Kind:  css_lexer.TEndOfFile,
			Range: logger.Range{Loc: p.tokens[p.end].Range.Loc},
		}
	}
	return css_lexer.Token{
		Kind:  css_lexer.TEndOfFile,
		Range: logger.Range{Loc: logger.Loc{Start: int32(len(p.source.Contents))}},
	}
}

func (p *parser) current() css_lexer.Token {
	return p.at(p.index)
}

func (p *parser) next() css_lexer.Token {
	return p.at(p.index + 1)
}

func (p *parser) raw() string {
	t := p.current()
	return p.source.Contents[t.Range.Loc.Start:t.Range.End()]
}

func (p *parser) decoded() string {
	return p.current().DecodedText(p.source.Contents)
}

func (p *parser) peek(kind css_lexer.T) bool {
	return kind == p.current().Kind
}

func (p *parser) eat(kind css_lexer.T) bool {
	if p.peek(kind) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expect(kind css_lexer.T) bool {
	if p.eat(kind) {
		return true
	}
	t := p.current()
	var text string
	if kind == css_lexer.TSemicolon && p.index > 0 && p.at(p.index-1).Kind == css_lexer.TWhitespace {
		// Have a nice error message for forgetting a trailing semicolon
		text = "Expected \";\""
		t = p.at(p.index - 1)
	} else {
		switch t.Kind {
		case css_lexer.TEndOfFile, css_lexer.TWhitespace:
			text = fmt.Sprintf("Expected %s but found %s", kind.String(), t.Kind.String())
			t.Range.Len = 0
		case css_lexer.TBadURL, css_lexer.TBadString:
			text = fmt.Sprintf("Expected %s but found %s", kind.String(), t.Kind.String())
		default:
			text = fmt.Sprintf("Expected %s but found %q", kind.String(), p.raw())
		}
	}
	if t.Range.Loc.Start > p.prevError.Start {
		p.log.AddRangeWarning(&p.tracker, t.Range, text)
		p.prevError = t.Range.Loc
	}
	return false
}

func (p *parser) unexpected() {
	if t := p.current(); t.Range.Loc.Start > p.prevError.Start {
		var text string
		switch t.Kind {
		case css_lexer.TEndOfFile, css_lexer.TWhitespace:
			text = fmt.Sprintf("Unexpected %s", t.Kind.String())
			t.Range.Len = 0
		case css_lexer.TBadURL, css_lexer.TBadString:
			text = fmt.Sprintf("Unexpected %s", t.Kind.String())
		default:
			text = fmt.Sprintf("Unexpected %q", p.raw())
		}
		p.log.AddRangeWarning(&p.tracker, t.Range, text)
		p.prevError = t.Range.Loc
	}
}

type ruleContext struct {
	isTopLevel     bool
	parseSelectors bool
}

func (p *parser) parseListOfRules(context ruleContext) []css_ast.R {
	didWarnAboutCharset := false
	didWarnAboutImport := false
	rules := []css_ast.R{}
	locs := []logger.Loc{}

loop:
	for {
		switch p.current().Kind {
		case css_lexer.TEndOfFile, css_lexer.TCloseBrace:
			break loop

		case css_lexer.TWhitespace:
			p.advance()
			continue

		case css_lexer.TAtKeyword:
			first := p.current().Range
			rule := p.parseAtRule(atRuleContext{})

			// Validate structure
			if context.isTopLevel {
				switch rule.(type) {
				case *css_ast.RAtCharset:
					if !didWarnAboutCharset && len(rules) > 0 {
						p.log.AddRangeWarningWithNotes(&p.tracker, first, "\"@charset\" must be the first rule in the file",
							[]logger.MsgData{logger.RangeData(&p.tracker, logger.Range{Loc: locs[len(locs)-1]},
								"This rule cannot come before a \"@charset\" rule")})
						didWarnAboutCharset = true
					}

				case *css_ast.RAtImport:
					if !didWarnAboutImport {
					importLoop:
						for i, before := range rules {
							switch before.(type) {
							case *css_ast.RAtCharset, *css_ast.RAtImport:
							default:
								p.log.AddRangeWarningWithNotes(&p.tracker, first, "All \"@import\" rules must come first",
									[]logger.MsgData{logger.RangeData(&p.tracker, logger.Range{Loc: locs[i]},
										"This rule cannot come before an \"@import\" rule")})
								didWarnAboutImport = true
								break importLoop
							}
						}
					}
				}
			}

			rules = append(rules, rule)
			if context.isTopLevel {
				locs = append(locs, first.Loc)
			}
			continue

		case css_lexer.TCDO, css_lexer.TCDC:
			if context.isTopLevel {
				p.advance()
				continue
			}
		}

		if context.isTopLevel {
			locs = append(locs, p.current().Range.Loc)
		}
		if context.parseSelectors {
			rules = append(rules, p.parseSelectorRule())
		} else {
			rules = append(rules, p.parseQualifiedRuleFrom(p.index, false /* isAlreadyInvalid */))
		}
	}

	if p.options.MangleSyntax {
		rules = removeEmptyAndDuplicateRules(rules)
	}
	return rules
}

func (p *parser) parseListOfDeclarations() (list []css_ast.R) {
	for {
		switch p.current().Kind {
		case css_lexer.TWhitespace, css_lexer.TSemicolon:
			p.advance()

		case css_lexer.TEndOfFile, css_lexer.TCloseBrace:
			list = p.processDeclarations(list)
			if p.options.MangleSyntax {
				list = removeEmptyAndDuplicateRules(list)
			}
			return

		case css_lexer.TAtKeyword:
			list = append(list, p.parseAtRule(atRuleContext{
				isDeclarationList: true,
			}))

		case css_lexer.TDelimAmpersand:
			// Reference: https://drafts.csswg.org/css-nesting-1/
			list = append(list, p.parseSelectorRule())

		default:
			list = append(list, p.parseDeclaration())
		}
	}
}

func removeEmptyAndDuplicateRules(rules []css_ast.R) []css_ast.R {
	type hashEntry struct {
		indices []uint32
	}

	n := len(rules)
	start := n
	entries := make(map[uint32]hashEntry)

	// Scan from the back so we keep the last rule
skipRule:
	for i := n - 1; i >= 0; i-- {
		rule := rules[i]

		switch r := rule.(type) {
		case *css_ast.RAtKeyframes:
			if len(r.Blocks) == 0 {
				continue
			}

		case *css_ast.RKnownAt:
			if len(r.Rules) == 0 {
				continue
			}

		case *css_ast.RSelector:
			if len(r.Rules) == 0 {
				continue
			}
		}

		if hash, ok := rule.Hash(); ok {
			entry := entries[hash]

			// For duplicate rules, omit all but the last copy
			for _, index := range entry.indices {
				if rule.Equal(rules[index]) {
					continue skipRule
				}
			}

			entry.indices = append(entry.indices, uint32(i))
			entries[hash] = entry
		}

		start--
		rules[start] = rule
	}

	return rules[start:]
}

func (p *parser) parseURLOrString() (string, logger.Range, bool) {
	t := p.current()
	switch t.Kind {
	case css_lexer.TString:
		text := p.decoded()
		p.advance()
		return text, t.Range, true

	case css_lexer.TURL:
		text := p.decoded()
		p.advance()
		return text, t.Range, true

	case css_lexer.TFunction:
		if p.decoded() == "url" {
			p.advance()
			t = p.current()
			text := p.decoded()
			if p.expect(css_lexer.TString) && p.expect(css_lexer.TCloseParen) {
				return text, t.Range, true
			}
		}
	}

	return "", logger.Range{}, false
}

func (p *parser) expectURLOrString() (url string, r logger.Range, ok bool) {
	url, r, ok = p.parseURLOrString()
	if !ok {
		p.expect(css_lexer.TURL)
	}
	return
}

type atRuleKind uint8

const (
	atRuleUnknown atRuleKind = iota
	atRuleDeclarations
	atRuleInheritContext
	atRuleEmpty
)

var specialAtRules = map[string]atRuleKind{
	"font-face": atRuleDeclarations,
	"page":      atRuleDeclarations,

	// These go inside "@page": https://www.w3.org/TR/css-page-3/#syntax-page-selector
	"bottom-center":       atRuleDeclarations,
	"bottom-left-corner":  atRuleDeclarations,
	"bottom-left":         atRuleDeclarations,
	"bottom-right-corner": atRuleDeclarations,
	"bottom-right":        atRuleDeclarations,
	"left-bottom":         atRuleDeclarations,
	"left-middle":         atRuleDeclarations,
	"left-top":            atRuleDeclarations,
	"right-bottom":        atRuleDeclarations,
	"right-middle":        atRuleDeclarations,
	"right-top":           atRuleDeclarations,
	"top-center":          atRuleDeclarations,
	"top-left-corner":     atRuleDeclarations,
	"top-left":            atRuleDeclarations,
	"top-right-corner":    atRuleDeclarations,
	"top-right":           atRuleDeclarations,

	// These properties are very deprecated and appear to only be useful for
	// mobile versions of internet explorer (which may no longer exist?), but
	// they are used by the https://ant.design/ design system so we recognize
	// them to avoid the warning.
	//
	//   Documentation: https://developer.mozilla.org/en-US/docs/Web/CSS/@viewport
	//   Discussion: https://github.com/w3c/csswg-drafts/issues/4766
	//
	"viewport":     atRuleDeclarations,
	"-ms-viewport": atRuleDeclarations,

	// This feature has been removed from the web because it's actively harmful.
	// However, there is one exception where "@-moz-document url-prefix() {" is
	// accepted by Firefox to basically be an "if Firefox" conditional rule.
	//
	//   Documentation: https://developer.mozilla.org/en-US/docs/Web/CSS/@document
	//   Discussion: https://bugzilla.mozilla.org/show_bug.cgi?id=1035091
	//
	"document":      atRuleInheritContext,
	"-moz-document": atRuleInheritContext,

	"media":    atRuleInheritContext,
	"scope":    atRuleInheritContext,
	"supports": atRuleInheritContext,
}

type atRuleContext struct {
	isDeclarationList bool
}

func (p *parser) parseAtRule(context atRuleContext) css_ast.R {
	// Parse the name
	atToken := p.decoded()
	atRange := p.current().Range
	kind := specialAtRules[atToken]
	p.advance()

	// Parse the prelude
	preludeStart := p.index
	switch atToken {
	case "charset":
		kind = atRuleEmpty
		p.expect(css_lexer.TWhitespace)
		if p.peek(css_lexer.TString) {
			encoding := p.decoded()
			if !strings.EqualFold(encoding, "UTF-8") {
				p.log.AddRangeWarning(&p.tracker, p.current().Range,
					fmt.Sprintf("\"UTF-8\" will be used instead of unsupported charset %q", encoding))
			}
			p.advance()
			p.expect(css_lexer.TSemicolon)
			return &css_ast.RAtCharset{Encoding: encoding}
		}
		p.expect(css_lexer.TString)

	case "import":
		kind = atRuleEmpty
		p.eat(css_lexer.TWhitespace)
		if path, r, ok := p.expectURLOrString(); ok {
			importConditionsStart := p.index
			for p.current().Kind != css_lexer.TSemicolon && p.current().Kind != css_lexer.TEndOfFile {
				p.parseComponentValue()
			}
			importConditions := p.convertTokens(p.tokens[importConditionsStart:p.index])
			kind := ast.ImportAt

			// Insert or remove whitespace before the first token
			if len(importConditions) > 0 {
				kind = ast.ImportAtConditional
				if p.options.RemoveWhitespace {
					importConditions[0].Whitespace &= ^css_ast.WhitespaceBefore
				} else {
					importConditions[0].Whitespace |= css_ast.WhitespaceBefore
				}
			}

			p.expect(css_lexer.TSemicolon)
			importRecordIndex := uint32(len(p.importRecords))
			p.importRecords = append(p.importRecords, ast.ImportRecord{
				Kind:  kind,
				Path:  logger.Path{Text: path},
				Range: r,
			})
			return &css_ast.RAtImport{
				ImportRecordIndex: importRecordIndex,
				ImportConditions:  importConditions,
			}
		}

	case "keyframes", "-webkit-keyframes", "-moz-keyframes", "-ms-keyframes", "-o-keyframes":
		p.eat(css_lexer.TWhitespace)
		var name string

		if p.peek(css_lexer.TIdent) {
			name = p.decoded()
			p.advance()
		} else if !p.expect(css_lexer.TIdent) && !p.eat(css_lexer.TString) && !p.peek(css_lexer.TOpenBrace) {
			// Consider string names a syntax error even though they are allowed by
			// the specification and they work in Firefox because they do not work in
			// Chrome or Safari.
			break
		}

		p.eat(css_lexer.TWhitespace)
		if p.expect(css_lexer.TOpenBrace) {
			var blocks []css_ast.KeyframeBlock

		blocks:
			for {
				switch p.current().Kind {
				case css_lexer.TWhitespace:
					p.advance()
					continue

				case css_lexer.TCloseBrace, css_lexer.TEndOfFile:
					break blocks

				case css_lexer.TOpenBrace:
					p.expect(css_lexer.TPercentage)
					p.parseComponentValue()

				default:
					var selectors []string

				selectors:
					for {
						t := p.current()
						switch t.Kind {
						case css_lexer.TWhitespace:
							p.advance()
							continue

						case css_lexer.TOpenBrace, css_lexer.TEndOfFile:
							break selectors

						case css_lexer.TIdent, css_lexer.TPercentage:
							text := p.decoded()
							if t.Kind == css_lexer.TIdent {
								if text == "from" {
									if p.options.MangleSyntax {
										text = "0%" // "0%" is equivalent to but shorter than "from"
									}
								} else if text != "to" {
									p.expect(css_lexer.TPercentage)
								}
							} else if p.options.MangleSyntax && text == "100%" {
								text = "to" // "to" is equivalent to but shorter than "100%"
							}
							selectors = append(selectors, text)
							p.advance()

						default:
							p.expect(css_lexer.TPercentage)
							p.parseComponentValue()
						}

						p.eat(css_lexer.TWhitespace)
						if t.Kind != css_lexer.TComma && !p.peek(css_lexer.TOpenBrace) {
							p.expect(css_lexer.TComma)
						}
					}

					if p.expect(css_lexer.TOpenBrace) {
						rules := p.parseListOfDeclarations()
						p.expect(css_lexer.TCloseBrace)

						// "@keyframes { from {} to { color: red } }" => "@keyframes { to { color: red } }"
						if !p.options.MangleSyntax || len(rules) > 0 {
							blocks = append(blocks, css_ast.KeyframeBlock{
								Selectors: selectors,
								Rules:     rules,
							})
						}
					}
				}
			}

			p.expect(css_lexer.TCloseBrace)
			return &css_ast.RAtKeyframes{
				AtToken: atToken,
				Name:    name,
				Blocks:  blocks,
			}
		}

	default:
		if kind == atRuleUnknown && atToken == "namespace" {
			// CSS namespaces are a weird feature that appears to only really be
			// useful for styling XML. And the world has moved on from XHTML to
			// HTML5 so pretty much no one uses CSS namespaces anymore. They are
			// also complicated to support in a bundler because CSS namespaces are
			// file-scoped, which means:
			//
			// * Default namespaces can be different in different files, in which
			//   case some default namespaces would have to be converted to prefixed
			//   namespaces to avoid collisions.
			//
			// * Prefixed namespaces from different files can use the same name, in
			//   which case some prefixed namespaces would need to be renamed to
			//   avoid collisions.
			//
			// Instead of implementing all of that for an extremely obscure feature,
			// CSS namespaces are just explicitly not supported.
			p.log.AddRangeWarning(&p.tracker, atRange, "\"@namespace\" rules are not supported")
		}
	}

	// Parse an unknown prelude
prelude:
	for {
		switch p.current().Kind {
		case css_lexer.TOpenBrace, css_lexer.TEndOfFile:
			break prelude

		case css_lexer.TSemicolon, css_lexer.TCloseBrace:
			prelude := p.convertTokens(p.tokens[preludeStart:p.index])

			// Report an error for rules that should have blocks
			if kind != atRuleEmpty && kind != atRuleUnknown {
				p.expect(css_lexer.TOpenBrace)
				p.eat(css_lexer.TSemicolon)
				return &css_ast.RUnknownAt{AtToken: atToken, Prelude: prelude}
			}

			// Otherwise, parse an unknown at rule
			p.expect(css_lexer.TSemicolon)
			return &css_ast.RUnknownAt{AtToken: atToken, Prelude: prelude}

		default:
			p.parseComponentValue()
		}
	}
	prelude := p.convertTokens(p.tokens[preludeStart:p.index])
	blockStart := p.index

	switch kind {
	case atRuleEmpty:
		// Report an error for rules that shouldn't have blocks
		p.expect(css_lexer.TSemicolon)
		p.parseBlock(css_lexer.TOpenBrace, css_lexer.TCloseBrace)
		block := p.convertTokens(p.tokens[blockStart:p.index])
		return &css_ast.RUnknownAt{AtToken: atToken, Prelude: prelude, Block: block}

	case atRuleDeclarations:
		// Parse known rules whose blocks consist of whatever the current context is
		p.advance()
		rules := p.parseListOfDeclarations()
		p.expect(css_lexer.TCloseBrace)
		return &css_ast.RKnownAt{AtToken: atToken, Prelude: prelude, Rules: rules}

	case atRuleInheritContext:
		// Parse known rules whose blocks consist of whatever the current context is
		p.advance()
		var rules []css_ast.R
		if context.isDeclarationList {
			rules = p.parseListOfDeclarations()
		} else {
			rules = p.parseListOfRules(ruleContext{
				parseSelectors: true,
			})
		}
		p.expect(css_lexer.TCloseBrace)
		return &css_ast.RKnownAt{AtToken: atToken, Prelude: prelude, Rules: rules}

	default:
		// Otherwise, parse an unknown rule
		p.parseBlock(css_lexer.TOpenBrace, css_lexer.TCloseBrace)
		block, _ := p.convertTokensHelper(p.tokens[blockStart:p.index], css_lexer.TEndOfFile, convertTokensOpts{allowImports: true})
		return &css_ast.RUnknownAt{AtToken: atToken, Prelude: prelude, Block: block}
	}
}

func (p *parser) convertTokens(tokens []css_lexer.Token) []css_ast.Token {
	result, _ := p.convertTokensHelper(tokens, css_lexer.TEndOfFile, convertTokensOpts{})
	return result
}

type convertTokensOpts struct {
	allowImports       bool
	verbatimWhitespace bool
}

func (p *parser) convertTokensHelper(tokens []css_lexer.Token, close css_lexer.T, opts convertTokensOpts) ([]css_ast.Token, []css_lexer.Token) {
	var result []css_ast.Token
	var nextWhitespace css_ast.WhitespaceFlags

loop:
	for len(tokens) > 0 {
		t := tokens[0]
		tokens = tokens[1:]
		if t.Kind == close {
			break loop
		}
		token := css_ast.Token{
			Kind:       t.Kind,
			Text:       t.DecodedText(p.source.Contents),
			Whitespace: nextWhitespace,
		}
		nextWhitespace = 0

		switch t.Kind {
		case css_lexer.TWhitespace:
			if last := len(result) - 1; last >= 0 {
				result[last].Whitespace |= css_ast.WhitespaceAfter
			}
			nextWhitespace = css_ast.WhitespaceBefore
			continue

		case css_lexer.TNumber:
			if p.options.MangleSyntax {
				if text, ok := mangleNumber(token.Text); ok {
					token.Text = text
				}
			}

		case css_lexer.TPercentage:
			if p.options.MangleSyntax {
				if text, ok := mangleNumber(token.PercentageValue()); ok {
					token.Text = text + "%"
				}
			}

		case css_lexer.TDimension:
			token.UnitOffset = t.UnitOffset

			if p.options.MangleSyntax {
				if text, ok := mangleNumber(token.DimensionValue()); ok {
					token.Text = text + token.DimensionUnit()
					token.UnitOffset = uint16(len(text))
				}

				if value, unit, ok := mangleDimension(token.DimensionValue(), token.DimensionUnit()); ok {
					token.Text = value + unit
					token.UnitOffset = uint16(len(value))
				}
			}

		case css_lexer.TURL:
			token.ImportRecordIndex = uint32(len(p.importRecords))
			p.importRecords = append(p.importRecords, ast.ImportRecord{
				Kind:     ast.ImportURL,
				Path:     logger.Path{Text: token.Text},
				Range:    t.Range,
				IsUnused: !opts.allowImports,
			})
			token.Text = ""

		case css_lexer.TFunction:
			var nested []css_ast.Token
			original := tokens
			nestedOpts := opts
			if token.Text == "var" {
				// CSS variables require verbatim whitespace for correctness
				nestedOpts.verbatimWhitespace = true
			}
			nested, tokens = p.convertTokensHelper(tokens, css_lexer.TCloseParen, nestedOpts)
			token.Children = &nested

			// Treat a URL function call with a string just like a URL token
			if token.Text == "url" && len(nested) == 1 && nested[0].Kind == css_lexer.TString {
				token.Kind = css_lexer.TURL
				token.Text = ""
				token.Children = nil
				token.ImportRecordIndex = uint32(len(p.importRecords))
				p.importRecords = append(p.importRecords, ast.ImportRecord{
					Kind:     ast.ImportURL,
					Path:     logger.Path{Text: nested[0].Text},
					Range:    original[0].Range,
					IsUnused: !opts.allowImports,
				})
			}

		case css_lexer.TOpenParen:
			var nested []css_ast.Token
			nested, tokens = p.convertTokensHelper(tokens, css_lexer.TCloseParen, opts)
			token.Children = &nested

		case css_lexer.TOpenBrace:
			var nested []css_ast.Token
			nested, tokens = p.convertTokensHelper(tokens, css_lexer.TCloseBrace, opts)

			// Pretty-printing: insert leading and trailing whitespace when not minifying
			if !opts.verbatimWhitespace && !p.options.RemoveWhitespace && len(nested) > 0 {
				nested[0].Whitespace |= css_ast.WhitespaceBefore
				nested[len(nested)-1].Whitespace |= css_ast.WhitespaceAfter
			}

			token.Children = &nested

		case css_lexer.TOpenBracket:
			var nested []css_ast.Token
			nested, tokens = p.convertTokensHelper(tokens, css_lexer.TCloseBracket, opts)
			token.Children = &nested
		}

		result = append(result, token)
	}

	if !opts.verbatimWhitespace {
		for i := range result {
			token := &result[i]

			// Always remove leading and trailing whitespace
			if i == 0 {
				token.Whitespace &= ^css_ast.WhitespaceBefore
			}
			if i+1 == len(result) {
				token.Whitespace &= ^css_ast.WhitespaceAfter
			}

			switch token.Kind {
			case css_lexer.TComma:
				// Assume that whitespace can always be removed before a comma
				token.Whitespace &= ^css_ast.WhitespaceBefore
				if i > 0 {
					result[i-1].Whitespace &= ^css_ast.WhitespaceAfter
				}

				// Assume whitespace can always be added after a comma
				if p.options.RemoveWhitespace {
					token.Whitespace &= ^css_ast.WhitespaceAfter
					if i+1 < len(result) {
						result[i+1].Whitespace &= ^css_ast.WhitespaceBefore
					}
				} else {
					token.Whitespace |= css_ast.WhitespaceAfter
					if i+1 < len(result) {
						result[i+1].Whitespace |= css_ast.WhitespaceBefore
					}
				}
			}
		}
	}

	// Insert an explicit whitespace token if we're in verbatim mode and all
	// tokens were whitespace. In this case there is no token to attach the
	// whitespace before/after flags so this is the only way to represent this.
	// This is the only case where this function generates an explicit whitespace
	// token. It represents whitespace as flags in all other cases.
	if opts.verbatimWhitespace && len(result) == 0 && nextWhitespace == css_ast.WhitespaceBefore {
		result = append(result, css_ast.Token{
			Kind: css_lexer.TWhitespace,
		})
	}

	return result, tokens
}

func shiftDot(text string, dotOffset int) (string, bool) {
	// This doesn't handle numbers with exponents
	if strings.ContainsAny(text, "eE") {
		return "", false
	}

	// Handle a leading sign
	sign := ""
	if len(text) > 0 && (text[0] == '-' || text[0] == '+') {
		sign = text[:1]
		text = text[1:]
	}

	// Remove the dot
	dot := strings.IndexByte(text, '.')
	if dot == -1 {
		dot = len(text)
	} else {
		text = text[:dot] + text[dot+1:]
	}

	// Move the dot
	dot += dotOffset

	// Remove any leading zeros before the dot
	for len(text) > 0 && dot > 0 && text[0] == '0' {
		text = text[1:]
		dot--
	}

	// Remove any trailing zeros after the dot
	for len(text) > 0 && len(text) > dot && text[len(text)-1] == '0' {
		text = text[:len(text)-1]
	}

	// Does this number have no fractional component?
	if dot >= len(text) {
		trailingZeros := strings.Repeat("0", dot-len(text))
		return fmt.Sprintf("%s%s%s", sign, text, trailingZeros), true
	}

	// Potentially add leading zeros
	if dot < 0 {
		text = strings.Repeat("0", -dot) + text
		dot = 0
	}

	// Insert the dot again
	return fmt.Sprintf("%s%s.%s", sign, text[:dot], text[dot:]), true
}

func mangleDimension(value string, unit string) (string, string, bool) {
	const msLen = 2
	const sLen = 1

	// Mangle times: https://developer.mozilla.org/en-US/docs/Web/CSS/time
	if strings.EqualFold(unit, "ms") {
		if shifted, ok := shiftDot(value, -3); ok && len(shifted)+sLen < len(value)+msLen {
			// Convert "ms" to "s" if shorter
			return shifted, "s", true
		}
	}
	if strings.EqualFold(unit, "s") {
		if shifted, ok := shiftDot(value, 3); ok && len(shifted)+msLen < len(value)+sLen {
			// Convert "s" to "ms" if shorter
			return shifted, "ms", true
		}
	}

	return "", "", false
}

func mangleNumber(t string) (string, bool) {
	original := t

	if dot := strings.IndexByte(t, '.'); dot != -1 {
		// Remove trailing zeros
		for len(t) > 0 && t[len(t)-1] == '0' {
			t = t[:len(t)-1]
		}

		// Remove the decimal point if it's unnecessary
		if dot+1 == len(t) {
			t = t[:dot]
			if t == "" || t == "+" || t == "-" {
				t += "0"
			}
		} else {
			// Remove a leading zero
			if len(t) >= 3 && t[0] == '0' && t[1] == '.' && t[2] >= '0' && t[2] <= '9' {
				t = t[1:]
			} else if len(t) >= 4 && (t[0] == '+' || t[0] == '-') && t[1] == '0' && t[2] == '.' && t[3] >= '0' && t[3] <= '9' {
				t = t[0:1] + t[2:]
			}
		}
	}

	return t, t != original
}

func (p *parser) parseSelectorRule() css_ast.R {
	preludeStart := p.index

	// Try parsing the prelude as a selector list
	if list, ok := p.parseSelectorList(); ok {
		rule := css_ast.RSelector{Selectors: list}
		if p.expect(css_lexer.TOpenBrace) {
			rule.Rules = p.parseListOfDeclarations()
			p.expect(css_lexer.TCloseBrace)
			return &rule
		}
	}

	// Otherwise, parse a generic qualified rule
	return p.parseQualifiedRuleFrom(preludeStart, true /* isAlreadyInvalid */)
}

func (p *parser) parseQualifiedRuleFrom(preludeStart int, isAlreadyInvalid bool) *css_ast.RQualified {
loop:
	for {
		switch p.current().Kind {
		case css_lexer.TOpenBrace, css_lexer.TEndOfFile:
			break loop

		case css_lexer.TSemicolon:
			// Error recovery if the block is omitted (likely some CSS meta-syntax)
			if !isAlreadyInvalid {
				p.expect(css_lexer.TOpenBrace)
			}
			prelude := p.convertTokens(p.tokens[preludeStart:p.index])
			p.advance()
			return &css_ast.RQualified{Prelude: prelude}

		default:
			p.parseComponentValue()
		}
	}

	rule := css_ast.RQualified{
		Prelude: p.convertTokens(p.tokens[preludeStart:p.index]),
	}

	if p.eat(css_lexer.TOpenBrace) {
		rule.Rules = p.parseListOfDeclarations()
		p.expect(css_lexer.TCloseBrace)
	} else if !isAlreadyInvalid {
		p.expect(css_lexer.TOpenBrace)
	}

	return &rule
}

func (p *parser) parseDeclaration() css_ast.R {
	// Parse the key
	keyStart := p.index
	ok := false
	if p.expect(css_lexer.TIdent) {
		p.eat(css_lexer.TWhitespace)
		if p.expect(css_lexer.TColon) {
			ok = true
		}
	}

	// Parse the value
	valueStart := p.index
stop:
	for {
		switch p.current().Kind {
		case css_lexer.TEndOfFile, css_lexer.TSemicolon, css_lexer.TCloseBrace:
			break stop

		case css_lexer.TOpenBrace:
			// Error recovery if there is an unexpected block (likely some CSS meta-syntax)
			p.parseComponentValue()
			p.eat(css_lexer.TWhitespace)
			if ok && !p.peek(css_lexer.TSemicolon) {
				p.expect(css_lexer.TSemicolon)
			}
			break stop

		default:
			p.parseComponentValue()
		}
	}

	// Stop now if this is not a valid declaration
	if !ok {
		return &css_ast.RBadDeclaration{
			Tokens: p.convertTokens(p.tokens[keyStart:p.index]),
		}
	}

	keyToken := p.tokens[keyStart]
	keyText := keyToken.DecodedText(p.source.Contents)
	value := p.tokens[valueStart:p.index]
	verbatimWhitespace := strings.HasPrefix(keyText, "--")

	// Remove trailing "!important"
	important := false
	i := len(value) - 1
	if i >= 0 && value[i].Kind == css_lexer.TWhitespace {
		i--
	}
	if i >= 0 && value[i].Kind == css_lexer.TIdent && strings.EqualFold(value[i].DecodedText(p.source.Contents), "important") {
		i--
		if i >= 0 && value[i].Kind == css_lexer.TWhitespace {
			i--
		}
		if i >= 0 && value[i].Kind == css_lexer.TDelimExclamation {
			value = value[:i]
			important = true
		}
	}

	result, _ := p.convertTokensHelper(value, css_lexer.TEndOfFile, convertTokensOpts{
		allowImports: true,

		// CSS variables require verbatim whitespace for correctness
		verbatimWhitespace: verbatimWhitespace,
	})

	// Insert or remove whitespace before the first token
	if !verbatimWhitespace && len(result) > 0 {
		if p.options.RemoveWhitespace {
			result[0].Whitespace &= ^css_ast.WhitespaceBefore
		} else {
			result[0].Whitespace |= css_ast.WhitespaceBefore
		}
	}

	return &css_ast.RDeclaration{
		Key:       css_ast.KnownDeclarations[keyText],
		KeyText:   keyText,
		KeyRange:  keyToken.Range,
		Value:     result,
		Important: important,
	}
}

func (p *parser) parseComponentValue() {
	switch p.current().Kind {
	case css_lexer.TFunction:
		p.parseBlock(css_lexer.TFunction, css_lexer.TCloseParen)

	case css_lexer.TOpenParen:
		p.parseBlock(css_lexer.TOpenParen, css_lexer.TCloseParen)

	case css_lexer.TOpenBrace:
		p.parseBlock(css_lexer.TOpenBrace, css_lexer.TCloseBrace)

	case css_lexer.TOpenBracket:
		p.parseBlock(css_lexer.TOpenBracket, css_lexer.TCloseBracket)

	case css_lexer.TEndOfFile:
		p.unexpected()

	default:
		p.advance()
	}
}

func (p *parser) parseBlock(open css_lexer.T, close css_lexer.T) {
	if p.expect(open) {
		for !p.eat(close) {
			if p.peek(css_lexer.TEndOfFile) {
				p.expect(close)
				return
			}

			p.parseComponentValue()
		}
	}
}
