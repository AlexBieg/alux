package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Send is one GitHub API request; on an HTTP error it still returns GitHub's answer.
type Send func(ctx context.Context, method, path string, body []byte) ([]byte, error)

func GhSend(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	args := []string{"api", "--method", method, path}
	if body != nil {
		args = append(args, "--input", "-")
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	if body != nil {
		cmd.Stdin = bytes.NewReader(body)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("gh: %s", msg)
		}
		return out, fmt.Errorf("gh: %w", err)
	}
	return out, nil
}

func sendJSON(ctx context.Context, send Send, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	raw, err := send(ctx, method, path, body)
	if err != nil {
		var resp struct {
			Message string `json:"message"`
			Errors  []any  `json:"errors"`
		}
		if json.Unmarshal(raw, &resp) == nil && resp.Message != "" {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(fmt.Sprint(resp.Message, " ", resp.Errors)))
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	return nil
}

// graphQL sends variables separately, so text from the model is never spliced into the query.
func graphQL(ctx context.Context, send Send, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	raw, sendErr := send(ctx, "POST", "graphql", body)
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Errors) > 0 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = e.Message
		}
		return errors.New("GitHub: " + strings.Join(msgs, "; "))
	}
	if sendErr != nil {
		return sendErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("prs: reading GitHub's answer: %w", err)
	}
	return nil
}
