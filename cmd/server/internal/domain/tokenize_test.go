package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchTokensSplitsCJKIntoBigramsAndNormalisesWidth(t *testing.T) {
	for text, want := range map[string][]string{
		"今年もCEATECに出展します":       {"今年", "年も", "ceatec", "に出", "出展", "展し", "しま", "ます"},
		"ＣＥＡＴＥＣ　２０２６ ブース":       {"ceatec", "2026", "ブー", "ース"},
		"Kubernetes の HPA 設定":   {"kubernetes", "の", "hpa", "設定"},
		"on-call runbook, v2.1": {"on", "call", "runbook", "v2", "1"},
		"サーバー":                  {"サー", "ーバ", "バー"},
		"   ":                   {},
		"한국어 테스트":               {"한국", "국어", "테스", "스트"},
	} {
		require.Equal(t, want, SearchTokens(text), text)
	}
	require.Equal(t, "ceatec 出展", SearchText("CEATEC 出展"))
}

func TestHashEmbeddingDistinguishesJapaneseTopics(t *testing.T) {
	ceatec := hashEmbedding("今年もCEATECに出展します。ブースはHall 5です。")
	same := hashEmbedding("CEATEC出展の準備について")
	other := hashEmbedding("社内の経費精算の締め切りは月末です。")
	dot := func(a, b []float32) float64 {
		s := 0.0
		for i := range a {
			s += float64(a[i] * b[i])
		}
		return s
	}
	require.Greater(t, dot(ceatec, same), dot(ceatec, other))
	require.Greater(t, dot(ceatec, same), 0.0, "the shared token 'ceatec' and bigrams must overlap")
}
