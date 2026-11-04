package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const threadsQuery = `query($owner:String!, $name:String!, $number:Int!, $cursor:String, $id:ID!, $commentsCursor:String, $fetchThreads:Boolean!, $fetchComments:Boolean!) { repository(owner:$owner,name:$name) @include(if:$fetchThreads) { pullRequest(number:$number) { reviewThreads(first:100, after:$cursor) { pageInfo { hasNextPage endCursor } nodes { id isResolved isOutdated path line comments(first:100) { pageInfo { hasNextPage endCursor } nodes { author { login } body createdAt } } } } } } node(id:$id) @include(if:$fetchComments) { ... on PullRequestReviewThread { comments(first:100, after:$commentsCursor) { pageInfo { hasNextPage endCursor } nodes { author { login } body createdAt } } } } }`

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
	valid       bool
}

func (p *pageInfo) UnmarshalJSON(data []byte) error {
	var v struct {
		HasNextPage *bool   `json:"hasNextPage"`
		EndCursor   *string `json:"endCursor"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if v.HasNextPage == nil {
		return fmt.Errorf("missing hasNextPage")
	}
	p.HasNextPage = *v.HasNextPage
	if v.EndCursor != nil {
		p.EndCursor = *v.EndCursor
	}
	p.valid = true
	return nil
}

type commentPage struct {
	PageInfo pageInfo          `json:"pageInfo"`
	Nodes    []json.RawMessage `json:"nodes"`
}
type reviewThread struct {
	ID         string      `json:"id"`
	IsResolved bool        `json:"isResolved"`
	IsOutdated bool        `json:"isOutdated"`
	Path       string      `json:"path"`
	Line       *int        `json:"line"`
	Comments   commentPage `json:"comments"`
}
type threadPage struct {
	PageInfo pageInfo       `json:"pageInfo"`
	Nodes    []reviewThread `json:"nodes"`
}
type graphqlResponse struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				ReviewThreads threadPage `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
		Node *struct {
			Comments commentPage `json:"comments"`
		} `json:"node"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

type graphqlFetch func(query string, fields ...string) ([]byte, error)

// writeThreads emits one JSON object per thread. Cursors and thread IDs are
// taken only from GitHub responses, never from the untrusted request.
func writeThreads(fetch graphqlFetch, out io.Writer, owner, repo string, number int) error {
	cursor := ""
	seen := map[string]bool{}
	for {
		fields := []string{"owner=" + owner, "name=" + repo, "number=" + strconv.Itoa(number), "id=0", "fetchThreads=true", "fetchComments=false"}
		if cursor != "" {
			fields = append(fields, "cursor="+cursor)
		}
		raw, err := fetch(threadsQuery, fields...)
		if err != nil {
			return err
		}
		var result graphqlResponse
		if err = json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("invalid thread response: %w", err)
		}
		if len(result.Errors) > 0 || result.Data.Repository == nil || result.Data.Repository.PullRequest == nil {
			return fmt.Errorf("GitHub did not return review threads")
		}
		page := result.Data.Repository.PullRequest.ReviewThreads
		if !page.PageInfo.valid || page.Nodes == nil {
			return fmt.Errorf("incomplete thread page")
		}
		for _, thread := range page.Nodes {
			if !thread.Comments.PageInfo.valid || thread.Comments.Nodes == nil {
				return fmt.Errorf("incomplete comment page")
			}
			seenComments := map[string]bool{}
			for thread.Comments.PageInfo.HasNextPage {
				next := thread.Comments.PageInfo.EndCursor
				if thread.ID == "" || next == "" || seenComments[next] {
					return fmt.Errorf("invalid comment pagination cursor")
				}
				seenComments[next] = true
				raw, err = fetch(threadsQuery, "owner="+owner, "name="+repo, "number="+strconv.Itoa(number), "id="+thread.ID, "commentsCursor="+next, "fetchThreads=false", "fetchComments=true")
				if err != nil {
					return err
				}
				var response graphqlResponse
				if err = json.Unmarshal(raw, &response); err != nil {
					return fmt.Errorf("invalid comment response: %w", err)
				}
				if len(response.Errors) > 0 || response.Data.Node == nil {
					return fmt.Errorf("GitHub did not return thread comments")
				}
				if !response.Data.Node.Comments.PageInfo.valid || response.Data.Node.Comments.Nodes == nil {
					return fmt.Errorf("incomplete comment page")
				}
				thread.Comments.Nodes = append(thread.Comments.Nodes, response.Data.Node.Comments.Nodes...)
				thread.Comments.PageInfo = response.Data.Node.Comments.PageInfo
				// Avoid holding unbounded comments before writing a single thread.
				if size, err := json.Marshal(thread); err != nil || len(size) > maxThreadBytes {
					return fmt.Errorf("thread exceeds output limit")
				}
			}
			b, err := json.Marshal(thread)
			if err != nil {
				return err
			}
			if len(b) > maxThreadBytes {
				return fmt.Errorf("thread exceeds output limit")
			}
			b = append(b, '\n')
			if _, err = io.Copy(out, bytes.NewReader(b)); err != nil {
				return err
			}
		}
		if !page.PageInfo.HasNextPage {
			return nil
		}
		next := page.PageInfo.EndCursor
		if next == "" || seen[next] {
			return fmt.Errorf("invalid thread pagination cursor")
		}
		seen[next] = true
		cursor = next
	}
}

const maxThreadBytes = 15 << 20

func threadQueryArgs(query string, fields ...string) ([]string, error) {
	if query != threadsQuery {
		return nil, fmt.Errorf("only the fixed thread query is allowed")
	}
	args := []string{"api", "graphql", "--method=POST", "--raw-field=query=" + threadsQuery}
	for _, field := range fields {
		key, _, ok := strings.Cut(field, "=")
		if !ok {
			return nil, fmt.Errorf("invalid GraphQL variable")
		}
		switch key {
		case "number", "fetchThreads", "fetchComments":
			args = append(args, "--field="+field)
		case "owner", "name", "id", "cursor", "commentsCursor":
			args = append(args, "--raw-field="+field)
		default:
			return nil, fmt.Errorf("invalid GraphQL variable")
		}
	}
	return args, nil
}
