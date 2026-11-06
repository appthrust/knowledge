package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Esa syncs posts from one esa.io team through the v1 REST API
// (https://docs.esa.io/posts/102). The list endpoint carries `body_md`, so a
// sync costs one request per 100 posts; esa allows 300 requests per user per
// 15 minutes.
type Esa struct {
	BaseURL string
	Client  *http.Client
}

// esaRequestLimit keeps one sync well inside esa's 15-minute window so a
// second source on the same token can still sync.
const esaRequestLimit = 60

var esaTeamPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func ValidEsaTeam(team string) bool { return esaTeamPattern.MatchString(team) }

// EsaQuery is the esa search string a source config resolves to: WIP posts
// (drafts) are excluded unless the source opts in.
func EsaQuery(config SourceConfig) string {
	q := strings.TrimSpace(config.Query)
	if !config.IncludeWIP {
		q += " wip:false"
	}
	if !config.IncludeArchived {
		// esa's "Archived" top-level category is its archive convention; such
		// posts crowd out current guidance in search unless the source opts in.
		q += " -in:Archived"
	}
	return strings.TrimSpace(q)
}

type esaPost struct {
	Number   int      `json:"number"`
	Name     string   `json:"name"`
	FullName string   `json:"full_name"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	WIP      bool     `json:"wip"`
	BodyMD   string   `json:"body_md"`
	URL      string   `json:"url"`
	Updated  string   `json:"updated_at"`
	Revision int      `json:"revision_number"`
	Stars    int      `json:"stargazers_count"`
	Watchers int      `json:"watchers_count"`
	Comments int      `json:"comments_count"`
}

// Sync emits only posts whose updated_at changed, keyed by post number. Posts
// that fail individually are recorded and the walk continues.
func (e *Esa) Sync(ctx context.Context, config SourceConfig, unchanged func(string, string) (bool, error), emit func(Document, string) error) ([]string, error) {
	errs := []string{}
	team := config.Team
	if !ValidEsaTeam(team) {
		return errs, errors.New("invalid esa team")
	}
	query := EsaQuery(config)
	page := 1
	for requests := 0; ; requests++ {
		if requests >= esaRequestLimit {
			return errs, errors.New("esa sync request limit exceeded")
		}
		params := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"100"}, "sort": {"number"}, "order": {"asc"}}
		if query != "" {
			params.Set("q", query)
		}
		var response struct {
			Posts []esaPost `json:"posts"`
			Next  *int      `json:"next_page"`
		}
		if err := e.request(ctx, config.Token, "/v1/teams/"+url.PathEscape(team)+"/posts?"+params.Encode(), &response); err != nil {
			return errs, err
		}
		for _, post := range response.Posts {
			if err := e.visit(post, unchanged, emit); err != nil {
				errs = append(errs, fmt.Sprintf("post %d: %s", post.Number, err))
			}
		}
		if response.Next == nil || *response.Next <= page {
			break
		}
		page = *response.Next
	}
	return errs, nil
}

func (e *Esa) visit(post esaPost, unchanged func(string, string) (bool, error), emit func(Document, string) error) error {
	if post.Number <= 0 {
		return errors.New("invalid esa post number")
	}
	id := strconv.Itoa(post.Number)
	same, err := unchanged(id, post.Updated)
	if err != nil || same {
		return err
	}
	if len(post.BodyMD) > MaxContentBytes {
		return errors.New("esa post exceeds content size limit")
	}
	// The full name carries the category path ("従業員用ガイド/勤怠/KOT/…"),
	// which is how esa itself labels a post and strong retrieval context.
	title := post.FullName
	if title == "" {
		title = post.Name
	}
	if title == "" {
		title = "#" + id
	}
	metadata := map[string]any{"number": post.Number, "wip": post.WIP, "updatedAt": post.Updated, "watchers": post.Watchers, "stars": post.Stars, "comments": post.Comments}
	if post.Category != "" {
		metadata["category"] = post.Category
	}
	if len(post.Tags) > 0 {
		metadata["tags"] = post.Tags
	}
	return emit(Document{ExternalID: id, Title: title, URL: post.URL, Content: post.BodyMD, Metadata: metadata}, post.Updated)
}

type EsaTeam struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
}

// Teams lists the teams a token can read (first page of GET /v1/teams); it is
// the connection probe behind the built-in app's "connected to esa.io" state.
func (e *Esa) Teams(ctx context.Context, token string) ([]EsaTeam, error) {
	var response struct {
		Teams []EsaTeam `json:"teams"`
	}
	if err := e.request(ctx, token, "/v1/teams?per_page=100", &response); err != nil {
		return nil, err
	}
	if response.Teams == nil {
		response.Teams = []EsaTeam{}
	}
	return response.Teams, nil
}

func (e *Esa) request(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := e.Client.Do(req)
	if err != nil {
		return errors.New("esa service unavailable")
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		if reset := res.Header.Get("X-RateLimit-Reset"); reset != "" {
			return fmt.Errorf("esa rate limit exceeded (resets at unix %s)", reset)
		}
		return errors.New("esa rate limit exceeded")
	default:
		return fmt.Errorf("esa returned HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(out); err != nil {
		return errors.New("invalid esa response")
	}
	return nil
}
