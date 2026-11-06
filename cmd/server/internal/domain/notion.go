package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Notion struct {
	BaseURL string
	Client  *http.Client
}
type notionObject struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Edited      string `json:"last_edited_time"`
	Archived    bool   `json:"archived"`
	HasChildren bool   `json:"has_children"`
	Properties  map[string]struct {
		Title []richText `json:"title"`
	} `json:"properties"`
	Raw map[string]json.RawMessage `json:"-"`
}

func (o *notionObject) UnmarshalJSON(b []byte) error {
	type plain notionObject
	if err := json.Unmarshal(b, (*plain)(o)); err != nil {
		return err
	}
	return json.Unmarshal(b, &o.Raw)
}

type richText struct {
	Plain string `json:"plain_text"`
	Text  struct {
		Content string `json:"content"`
		Link    *struct {
			URL string `json:"url"`
		} `json:"link"`
	} `json:"text"`
	Annotations struct{ Bold, Italic, Code bool } `json:"annotations"`
}

func rich(items []richText) string {
	var b strings.Builder
	for _, r := range items {
		s := r.Plain
		if s == "" {
			s = r.Text.Content
		}
		if r.Annotations.Code {
			s = "`" + s + "`"
		}
		if r.Annotations.Bold {
			s = "**" + s + "**"
		}
		if r.Annotations.Italic {
			s = "*" + s + "*"
		}
		if r.Text.Link != nil {
			s = "[" + s + "](" + r.Text.Link.URL + ")"
		}
		b.WriteString(s)
	}
	return b.String()
}

type notionRun struct {
	n                *Notion
	ctx              context.Context
	token            string
	requests, blocks int
	seen             map[string]bool
}

func (r *notionRun) request(method, path string, body any, out any) error {
	r.requests++
	if r.requests > 500 {
		return errors.New("Notion sync request limit exceeded")
	}
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(r.ctx, method, r.n.BaseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Notion-Version", "2022-06-28")
	req.Header.Set("Content-Type", "application/json")
	res, err := r.n.Client.Do(req)
	if err != nil {
		return errors.New("Notion service unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Notion returned HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); err != nil {
		return errors.New("invalid Notion response")
	}
	return nil
}
func (r *notionRun) list(method, path string, body map[string]any, visit func(notionObject) error) error {
	cursor := ""
	seen := map[string]bool{}
	for {
		p := path
		payload := map[string]any{"page_size": 100}
		for k, v := range body {
			payload[k] = v
		}
		if method == "GET" {
			p += "?page_size=100"
			if cursor != "" {
				p += "&start_cursor=" + url.QueryEscape(cursor)
			}
		} else if cursor != "" {
			payload["start_cursor"] = cursor
		}
		var response struct {
			Results []notionObject `json:"results"`
			More    bool           `json:"has_more"`
			Next    string         `json:"next_cursor"`
		}
		var requestBody any = payload
		if method == "GET" {
			requestBody = nil
		}
		if err := r.request(method, p, requestBody, &response); err != nil {
			return err
		}
		for _, o := range response.Results {
			if err := visit(o); err != nil {
				return err
			}
		}
		if !response.More {
			return nil
		}
		if response.Next == "" || seen[response.Next] {
			return errors.New("invalid Notion pagination cursor")
		}
		seen[response.Next] = true
		cursor = response.Next
	}
}

// Sync emits only pages whose last_edited_time changed. The caller persists that
// revision with the document, so failed upserts are retried on the next sync.
func (n *Notion) Sync(ctx context.Context, config SourceConfig, unchanged func(string, string) (bool, error), emit func(Document, string) error) ([]string, error) {
	r := &notionRun{n: n, ctx: ctx, token: config.Token, seen: map[string]bool{}}
	errs := []string{}
	var visit func(notionObject) error
	// A page failure is reported without stopping discovery of other pages.
	recordPage := func(page notionObject) error {
		if err := visit(page); err != nil {
			errs = append(errs, page.ID+": "+err.Error())
		}
		return nil
	}
	visit = func(page notionObject) error {
		if page.Archived || r.seen[page.ID] {
			return nil
		}
		r.seen[page.ID] = true
		if len(r.seen) > 200 {
			return errors.New("Notion sync page limit exceeded")
		}
		if page.Object == "database" {
			return r.list("POST", "/v1/databases/"+url.PathEscape(page.ID)+"/query", nil, recordPage)
		}
		same, err := unchanged(page.ID, page.Edited)
		if err != nil {
			return err
		}
		// Search enumerates descendant pages independently. Explicit roots do
		// not: walk their blocks even when unchanged to discover edited child
		// pages/databases, but do not re-embed the unchanged parent.
		if same && len(config.RootPageIDs) == 0 {
			return nil
		}
		content, children, err := r.blocksMarkdown(page.ID, 0)
		if err != nil {
			return err
		}
		title := page.ID
		for _, p := range page.Properties {
			if len(p.Title) > 0 {
				title = rich(p.Title)
				break
			}
		}
		if len(content) > MaxContentBytes {
			return errors.New("Notion document exceeds content size limit")
		}
		if !same {
			if err := emit(Document{ExternalID: page.ID, Title: title, URL: page.URL, Content: content}, page.Edited); err != nil {
				return err
			}
		}
		for _, child := range children {
			if child.Object == "database" {
				if err := visit(child); err != nil {
					return err
				}
				continue
			}
			var p notionObject
			if err := r.request("GET", "/v1/pages/"+url.PathEscape(child.ID), nil, &p); err != nil {
				return err
			}
			if err := visit(p); err != nil {
				return err
			}
		}
		return nil
	}
	if len(config.RootPageIDs) == 0 && len(config.DatabaseIDs) == 0 {
		err := r.list("POST", "/v1/search", map[string]any{"filter": map[string]string{"value": "page", "property": "object"}}, recordPage)
		return errs, err
	}
	for _, id := range config.RootPageIDs {
		var p notionObject
		if err := r.request("GET", "/v1/pages/"+url.PathEscape(id), nil, &p); err != nil {
			return errs, err
		}
		_ = recordPage(p)
	}
	for _, id := range config.DatabaseIDs {
		if err := visit(notionObject{ID: id, Object: "database"}); err != nil {
			return errs, err
		}
	}
	return errs, nil
}
func (r *notionRun) blocksMarkdown(id string, depth int) (string, []notionObject, error) {
	if depth > 32 {
		return "", nil, errors.New("Notion block depth limit exceeded")
	}
	var b strings.Builder
	children := []notionObject{}
	err := r.list("GET", "/v1/blocks/"+url.PathEscape(id)+"/children", nil, func(block notionObject) error {
		r.blocks++
		if r.blocks > 10000 {
			return errors.New("Notion block limit exceeded")
		}
		if block.Archived {
			return nil
		}
		var data struct {
			Rich     []richText   `json:"rich_text"`
			Title    string       `json:"title"`
			Language string       `json:"language"`
			Checked  bool         `json:"checked"`
			URL      string       `json:"url"`
			Caption  []richText   `json:"caption"`
			Cells    [][]richText `json:"cells"`
			External struct {
				URL string `json:"url"`
			} `json:"external"`
			File struct {
				URL string `json:"url"`
			} `json:"file"`
			Expression string `json:"expression"`
		}
		if raw := block.Raw[block.Type]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &data); err != nil {
				return errors.New("invalid Notion block")
			}
		}
		text := rich(data.Rich)
		switch block.Type {
		case "heading_1":
			text = "# " + text
		case "heading_2":
			text = "## " + text
		case "heading_3":
			text = "### " + text
		case "bulleted_list_item":
			text = "- " + text
		case "numbered_list_item":
			text = "1. " + text
		case "to_do":
			mark := " "
			if data.Checked {
				mark = "x"
			}
			text = "- [" + mark + "] " + text
		case "quote", "callout":
			text = "> " + text
		case "code":
			text = "```" + data.Language + "\n" + text + "\n```"
		case "divider":
			text = "---"
		case "equation":
			text = "$$" + data.Expression + "$$"
		case "table_row":
			cells := make([]string, len(data.Cells))
			for i, c := range data.Cells {
				cells[i] = rich(c)
			}
			text = "| " + strings.Join(cells, " | ") + " |"
		case "bookmark", "embed", "link_preview":
			text = data.URL
		case "image", "file", "pdf", "video", "audio":
			u := data.External.URL
			if u == "" {
				u = data.File.URL
			}
			text = "[" + rich(data.Caption) + "](" + u + ")"
			if block.Type == "image" {
				text = "!" + text
			}
		case "child_page", "child_database":
			kind := "page"
			if block.Type == "child_database" {
				kind = "database"
			}
			children = append(children, notionObject{ID: block.ID, Object: kind})
			text = data.Title
		}
		if text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		}
		if block.HasChildren && block.Type != "child_page" && block.Type != "child_database" {
			nested, pages, err := r.blocksMarkdown(block.ID, depth+1)
			if err != nil {
				return err
			}
			b.WriteString(nested)
			children = append(children, pages...)
		}
		if b.Len() > MaxContentBytes {
			return errors.New("Notion document exceeds content size limit")
		}
		return nil
	})
	return strings.TrimSpace(b.String()), children, err
}
