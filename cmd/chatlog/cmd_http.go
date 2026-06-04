package chatlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

type httpEndpoint struct {
	Name        string
	Method      string
	Path        string
	Description string
}

var (
	httpAddr      string
	httpMethod    string
	httpPath      string
	httpEndpointN string
	httpQueryKVs  []string
	httpHeaderKVs []string
	httpPathKVs   []string
	httpBody      string
	httpBodyFile  string
	httpTimeoutS  int
	httpInsecure  bool
	httpShowCode  bool
	httpOutput    string

	httpCmd = &cobra.Command{
		Use:   "http",
		Short: "Call built-in HTTP APIs from CLI",
	}
	httpListCmd = &cobra.Command{
		Use:   "list",
		Short: "List all supported HTTP endpoints",
		Run:   runHTTPList,
	}
	httpCallCmd = &cobra.Command{
		Use:   "call",
		Short: "Call an HTTP endpoint",
		Long: `Call any chatlog HTTP endpoint.

Examples:
  chatlog http list
  chatlog http call --endpoint history --query chat=xxx --query limit=100
  chatlog http call --path /api/v1/db/query --query group=message --query file=message_0.db --query sql='select count(*) c from MSG'
  chatlog http call --endpoint image --path-param key=b309f8f81716aff9e1c9dded3bec74e7
  chatlog http call --endpoint cache_clear --method POST`,
		Run: runHTTPCall,
	}
)

var httpEndpoints = []httpEndpoint{
	{Name: "health", Method: "GET", Path: "/health", Description: "Health check"},
	{Name: "ping", Method: "GET", Path: "/api/v1/ping", Description: "Ping"},

	{Name: "sessions", Method: "GET", Path: "/api/v1/sessions", Description: "Session list"},
	{Name: "history", Method: "GET", Path: "/api/v1/history", Description: "Chat history"},
	{Name: "search", Method: "GET", Path: "/api/v1/search", Description: "Message search"},
	{Name: "unread", Method: "GET", Path: "/api/v1/unread", Description: "Unread chats"},
	{Name: "members", Method: "GET", Path: "/api/v1/members", Description: "Chatroom members"},
	{Name: "new_messages", Method: "GET", Path: "/api/v1/new_messages", Description: "Incremental messages"},
	{Name: "stats", Method: "GET", Path: "/api/v1/stats", Description: "Chat stats"},
	{Name: "favorites", Method: "GET", Path: "/api/v1/favorites", Description: "Favorites"},
	{Name: "sns_notifications", Method: "GET", Path: "/api/v1/sns_notifications", Description: "SNS notifications"},
	{Name: "sns_feed", Method: "GET", Path: "/api/v1/sns_feed", Description: "SNS feed"},
	{Name: "sns_search", Method: "GET", Path: "/api/v1/sns_search", Description: "SNS search"},
	{Name: "contacts", Method: "GET", Path: "/api/v1/contacts", Description: "Contact list"},
	{Name: "chatrooms", Method: "GET", Path: "/api/v1/chatrooms", Description: "Chatroom list"},
	{Name: "db", Method: "GET", Path: "/api/v1/db", Description: "Databases"},
	{Name: "db_tables", Method: "GET", Path: "/api/v1/db/tables", Description: "Database tables"},
	{Name: "db_data", Method: "GET", Path: "/api/v1/db/data", Description: "Table data"},
	{Name: "db_query", Method: "GET", Path: "/api/v1/db/query", Description: "Execute SQL"},
	{Name: "cache_clear", Method: "POST", Path: "/api/v1/cache/clear", Description: "Clear cache"},

	{Name: "image", Method: "GET", Path: "/image/{key}", Description: "Get image media"},
	{Name: "video", Method: "GET", Path: "/video/{key}", Description: "Get video media"},
	{Name: "file", Method: "GET", Path: "/file/{key}", Description: "Get file media"},
	{Name: "voice", Method: "GET", Path: "/voice/{key}", Description: "Get voice media"},
	{Name: "data", Method: "GET", Path: "/data/{path}", Description: "Read raw data path"},

	{Name: "mcp", Method: "POST", Path: "/mcp", Description: "MCP streamable HTTP endpoint"},
	{Name: "mcp_sse", Method: "GET", Path: "/sse", Description: "MCP SSE endpoint"},
	{Name: "mcp_message", Method: "POST", Path: "/message", Description: "MCP SSE message endpoint"},
}

func init() {
	rootCmd.AddCommand(httpCmd)
	httpCmd.AddCommand(httpListCmd)
	httpCmd.AddCommand(httpCallCmd)

	httpCmd.PersistentFlags().StringVarP(&httpAddr, "addr", "a", "127.0.0.1:5030", "http server address, e.g. 127.0.0.1:5030")
	httpCmd.PersistentFlags().IntVar(&httpTimeoutS, "timeout", 30, "request timeout in seconds")
	httpCmd.PersistentFlags().BoolVar(&httpInsecure, "insecure", false, "use http (default true for chatlog); kept for compatibility")
	httpCmd.PersistentFlags().BoolVar(&httpShowCode, "show-status", true, "print HTTP status line before body")

	httpCallCmd.Flags().StringVar(&httpEndpointN, "endpoint", "", "endpoint alias, use chatlog http list to inspect")
	httpCallCmd.Flags().StringVar(&httpPath, "path", "", "raw path, e.g. /api/v1/history")
	httpCallCmd.Flags().StringVarP(&httpMethod, "method", "X", "", "HTTP method override")
	httpCallCmd.Flags().StringArrayVar(&httpQueryKVs, "query", nil, "query key=value (repeatable)")
	httpCallCmd.Flags().StringArrayVar(&httpHeaderKVs, "header", nil, "header key=value (repeatable)")
	httpCallCmd.Flags().StringArrayVar(&httpPathKVs, "path-param", nil, "path param key=value for template path")
	httpCallCmd.Flags().StringVar(&httpBody, "body", "", "raw request body")
	httpCallCmd.Flags().StringVar(&httpBodyFile, "body-file", "", "request body file path")
	httpCallCmd.Flags().StringVarP(&httpOutput, "output", "o", "", "save response body to file")
}

func runHTTPList(cmd *cobra.Command, args []string) {
	items := append([]httpEndpoint(nil), httpEndpoints...)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	for _, ep := range items {
		fmt.Printf("%-18s %-6s %-30s %s\n", ep.Name, ep.Method, ep.Path, ep.Description)
	}
}

func runHTTPCall(cmd *cobra.Command, args []string) {
	ep, found := resolveEndpoint(httpEndpointN)
	method := strings.TrimSpace(strings.ToUpper(httpMethod))
	path := strings.TrimSpace(httpPath)

	if found {
		if path == "" {
			path = ep.Path
		}
		if method == "" {
			method = ep.Method
		}
	}
	if path == "" {
		log.Error().Msg("path is required, use --path or --endpoint")
		return
	}
	if method == "" {
		method = http.MethodGet
	}

	pathParams, err := parseKVList(httpPathKVs)
	if err != nil {
		log.Error().Err(err).Msg("invalid --path-param")
		return
	}
	path = applyPathTemplate(path, pathParams)

	queryVals, err := parseKVListToValues(httpQueryKVs)
	if err != nil {
		log.Error().Err(err).Msg("invalid --query")
		return
	}
	if qs := queryVals.Encode(); qs != "" {
		if strings.Contains(path, "?") {
			path += "&" + qs
		} else {
			path += "?" + qs
		}
	}

	addr := strings.TrimSpace(httpAddr)
	if addr == "" {
		log.Error().Msg("addr is required")
		return
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		addr = strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	}
	base := "http://"
	if !httpInsecure {
		base = "http://"
	}
	fullURL := base + addr + path

	bodyReader, err := buildRequestBody()
	if err != nil {
		log.Error().Err(err).Msg("build request body failed")
		return
	}

	req, err := http.NewRequest(method, fullURL, bodyReader)
	if err != nil {
		log.Error().Err(err).Msg("create request failed")
		return
	}
	headers, err := parseKVList(httpHeaderKVs)
	if err != nil {
		log.Error().Err(err).Msg("invalid --header")
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{}
	if httpTimeoutS > 0 {
		client.Timeout = timeSeconds(httpTimeoutS)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Error().Err(err).Msg("request failed")
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error().Err(err).Msg("read response failed")
		return
	}

	if httpShowCode {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Status)
	}
	if httpOutput != "" {
		if err := os.WriteFile(httpOutput, respBody, 0o644); err != nil {
			log.Error().Err(err).Msg("write output file failed")
			return
		}
		fmt.Fprintln(os.Stderr, "saved:", httpOutput)
		return
	}
	fmt.Print(string(respBody))
	if len(respBody) > 0 && respBody[len(respBody)-1] != '\n' {
		fmt.Println()
	}
}

func resolveEndpoint(name string) (httpEndpoint, bool) {
	n := strings.TrimSpace(strings.ToLower(name))
	if n == "" {
		return httpEndpoint{}, false
	}
	for _, ep := range httpEndpoints {
		if strings.ToLower(ep.Name) == n {
			return ep, true
		}
	}
	return httpEndpoint{}, false
}

func parseKVList(items []string) (map[string]string, error) {
	out := make(map[string]string, len(items))
	for _, item := range items {
		k, v, err := parseKV(item)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func parseKVListToValues(items []string) (url.Values, error) {
	out := url.Values{}
	for _, item := range items {
		k, v, err := parseKV(item)
		if err != nil {
			return nil, err
		}
		out.Add(k, v)
	}
	return out, nil
}

func parseKV(raw string) (string, string, error) {
	x := strings.TrimSpace(raw)
	if x == "" {
		return "", "", fmt.Errorf("empty key=value")
	}
	i := strings.IndexByte(x, '=')
	if i <= 0 {
		return "", "", fmt.Errorf("invalid key=value: %s", raw)
	}
	key := strings.TrimSpace(x[:i])
	val := strings.TrimSpace(x[i+1:])
	if key == "" {
		return "", "", fmt.Errorf("empty key in: %s", raw)
	}
	return key, val, nil
}

func applyPathTemplate(path string, kv map[string]string) string {
	out := path
	for k, v := range kv {
		placeholder := "{" + k + "}"
		out = strings.ReplaceAll(out, placeholder, url.PathEscape(v))
	}
	return out
}

func buildRequestBody() (io.Reader, error) {
	if strings.TrimSpace(httpBodyFile) != "" {
		b, err := os.ReadFile(strings.TrimSpace(httpBodyFile))
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(b), nil
	}
	if strings.TrimSpace(httpBody) == "" {
		return nil, nil
	}
	raw := strings.TrimSpace(httpBody)
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		var anyJSON interface{}
		if err := json.Unmarshal([]byte(raw), &anyJSON); err != nil {
			return nil, fmt.Errorf("invalid json body: %w", err)
		}
	}
	return strings.NewReader(httpBody), nil
}

func timeSeconds(s int) timeDuration {
	if s <= 0 {
		return 0
	}
	return timeDuration(s) * timeSecond
}

type timeDuration = time.Duration

const timeSecond = time.Second
