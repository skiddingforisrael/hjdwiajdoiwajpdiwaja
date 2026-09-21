package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type robloxCredentials struct {
	APIKey  string
	UserID  string
	GroupID string
}

// loadRobloxCredentials supports environment variables and the requested
// .env.go file. Go intentionally ignores dot-prefixed source files, so the file
// is parsed as configuration rather than compiled into the executable.
func loadRobloxCredentials() (robloxCredentials, error) {
	credentials := robloxCredentials{
		APIKey:  strings.TrimSpace(os.Getenv("ROBLOX_API_KEY")),
		UserID:  strings.TrimSpace(os.Getenv("ROBLOX_USER_ID")),
		GroupID: strings.TrimSpace(os.Getenv("ROBLOX_GROUP_ID")),
	}
	if credentials.APIKey != "" && ((credentials.UserID != "") != (credentials.GroupID != "")) {
		return credentials, nil
	}
	paths := []string{".env.go"}
	if executable, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(executable), ".env.go"))
	}
	var configPath string
	for _, candidate := range paths {
		if _, err := os.Stat(candidate); err == nil {
			configPath = candidate
			break
		}
	}
	if configPath == "" {
		return robloxCredentials{}, errors.New("Roblox credentials not found; add RobloxAPIKey and RobloxUserID to .env.go")
	}
	values, err := parseGoConfig(configPath)
	if err != nil {
		return robloxCredentials{}, err
	}
	if credentials.APIKey == "" {
		credentials.APIKey = values["RobloxAPIKey"]
	}
	if credentials.UserID == "" {
		credentials.UserID = values["RobloxUserID"]
	}
	if credentials.GroupID == "" {
		credentials.GroupID = values["RobloxGroupID"]
	}
	if credentials.APIKey == "" {
		return robloxCredentials{}, errors.New("RobloxAPIKey is empty in .env.go")
	}
	if (credentials.UserID != "") == (credentials.GroupID != "") {
		return robloxCredentials{}, errors.New("set exactly one of RobloxUserID or RobloxGroupID in .env.go")
	}
	return credentials, nil
}

func loadDiscordWebhookURL() (string, error) {
	if value := strings.TrimSpace(os.Getenv("CONE_DISCORD_WEBHOOK_URL")); value != "" {
		return value, nil
	}
	paths := []string{".env.go"}
	if executable, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(executable), ".env.go"))
	}
	for _, candidate := range paths {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		values, err := parseGoConfig(candidate)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(values["DiscordWebhookURL"]), nil
	}
	return "", nil
}

func parseGoConfig(path string) (map[string]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	values := make(map[string]string)
	ast.Inspect(parsed, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for index, name := range spec.Names {
			if index >= len(spec.Values) {
				continue
			}
			literal, ok := spec.Values[index].(*ast.BasicLit)
			if !ok {
				continue
			}
			value := literal.Value
			if literal.Kind == token.STRING {
				if unquoted, unquoteErr := strconv.Unquote(value); unquoteErr == nil {
					value = unquoted
				}
			}
			values[name.Name] = strings.TrimSpace(value)
		}
		return true
	})
	return values, nil
}
