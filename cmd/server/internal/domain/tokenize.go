package domain

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// SearchTokens turns text into the lexical index/query tokens. Postgres'
// `simple` configuration splits on whitespace and punctuation only, so a
// Japanese sentence ("今年もCEATECに出展します") would be one token and never
// match "ceatec". Here text is NFKC-normalised (full-width → ASCII) and
// lower-cased; runs of CJK characters become character bigrams, every other
// letter/digit run is one word. The same function tokenises queries, so a
// query is a conjunction of its words and bigrams.
func SearchTokens(text string) []string {
	text = strings.ToLower(norm.NFKC.String(text))
	tokens := []string{}
	var word, cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			tokens = append(tokens, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			tokens = append(tokens, string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				tokens = append(tokens, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return tokens
}

// SearchText is the space-joined token form stored in `chunks.search_text`.
func SearchText(text string) string {
	return strings.Join(SearchTokens(text), " ")
}

// SearchQueries returns the lexical query plan for `to_tsquery('simple', …)`:
// first a strict form where every whitespace/punctuation-separated term must
// appear as a phrase of its tokens (consecutive bigrams = substring match, so
// "控除証明書" matches exactly that string) and all terms are required; then
// a loose form OR-ing every token, for natural-language queries whose long
// phrases never occur verbatim. Tokens are quoted so operator characters in
// the query cannot alter the plan.
func SearchQueries(text string) (strict, loose string) {
	var phrases, all []string
	for _, term := range strings.FieldsFunc(strings.ToLower(norm.NFKC.String(text)), func(r rune) bool {
		return !isCJK(r) && !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		tokens := SearchTokens(term)
		if len(tokens) == 0 {
			continue
		}
		quoted := make([]string, len(tokens))
		for i, token := range tokens {
			quoted[i] = "'" + strings.ReplaceAll(token, "'", "''") + "'"
		}
		all = append(all, quoted...)
		phrases = append(phrases, "("+strings.Join(quoted, " <-> ")+")")
	}
	return strings.Join(phrases, " & "), strings.Join(all, " | ")
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) || r == 'ー'
}
