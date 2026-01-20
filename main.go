package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type Source struct {
	Name string
	Code string
}

type TreeNode struct {
	Name        string      // Just the name segment
	FullPath    string      // Full path for context
	IsDirectory bool        // true for folders, false for files
	IsExpanded  bool        // whether folder is expanded
	Children    []*TreeNode // child nodes (empty for files)
	Parent      *TreeNode   // parent node (nil for root)
	SourceIdx   int         // index into sources array (-1 for directories)
	Depth       int         // depth in tree (0 for root level)
}

type FlatTreeItem struct {
	Node   *TreeNode
	Index  int
	IsLast bool
}

type model struct {
	state           string        // "loading", "error", or "view"
	url             string
	sourcemapURL    string        // Optional direct sourcemap URL
	headers         map[string]string
	error           string
	sources         []Source
	selectedIdx     int
	codeViewport    int
	listViewport    int // Scroll position for the list
	width           int
	height          int
	searchMode      bool
	searchQuery     string
	searchMatches   []int // Line numbers of matches
	currentMatch    int   // Index into searchMatches
	mouseMode       bool  // Toggle for mouse interaction vs text selection
	fileTree        *TreeNode
	flatTree        []FlatTreeItem
	selectedTreePos int
}

func initialModel(url string, sourcemapURL string, headers map[string]string) model {
	return model{
		state:        "loading",
		url:          url,
		sourcemapURL: sourcemapURL,
		headers:      headers,
		sources:      []Source{},
		selectedIdx:  0,
		mouseMode:    true, // Start with mouse mode enabled
	}
}

func (m model) Init() tea.Cmd {
	return func() tea.Msg {
		// Set terminal title to "unmap <dot> url"
		title := fmt.Sprintf("unmap • %s", m.url)
		fmt.Fprint(os.Stderr, "\033]0;"+title+"\007")

		// If sourcemapURL is provided, fetch directly from it
		if m.sourcemapURL != "" {
			sources, err := fetchSourcemapFromURL(m.sourcemapURL, m.headers)
			return fetchedSourcesMsg{sources: sources, err: err}
		}

		return fetchSourcesMsg(m.url, m.headers)
	}
}

func buildFileTree(sources []Source) *TreeNode {
	root := &TreeNode{
		Name:        "",
		FullPath:    "",
		IsDirectory: true,
		IsExpanded:  true,
		Children:    make([]*TreeNode, 0),
		Parent:      nil,
		SourceIdx:   -1,
		Depth:       0,
	}

	for i, src := range sources {
		pathParts := strings.Split(src.Name, "/")
		// Filter out empty parts, ".", and ".."
		var cleanParts []string
		for _, part := range pathParts {
			if part != "" && part != "." && part != ".." {
				cleanParts = append(cleanParts, part)
			}
		}
		if len(cleanParts) > 0 {
			insertIntoTree(root, cleanParts, i, 0)
		}
	}

	return root
}

func insertIntoTree(node *TreeNode, pathParts []string, sourceIdx int, depth int) {
	if len(pathParts) == 0 {
		return
	}

	currentPart := pathParts[0]
	isLeaf := len(pathParts) == 1

	// Find or create child
	var childNode *TreeNode
	for _, child := range node.Children {
		if child.Name == currentPart {
			childNode = child
			break
		}
	}

	if childNode == nil {
		childNode = &TreeNode{
			Name:        currentPart,
			FullPath:    node.FullPath + "/" + currentPart,
			IsDirectory: !isLeaf,
			IsExpanded:  true,
			Children:    make([]*TreeNode, 0),
			Parent:      node,
			SourceIdx:   -1,
			Depth:       depth + 1,
		}
		if isLeaf {
			childNode.SourceIdx = sourceIdx
		}
		node.Children = append(node.Children, childNode)
	}

	if isLeaf {
		return
	}

	// Recurse for remaining path parts
	insertIntoTree(childNode, pathParts[1:], sourceIdx, depth+1)
}

func sortChildren(children []*TreeNode) {
	// Simple bubble sort for alphabetical order
	for i := 0; i < len(children); i++ {
		for j := i + 1; j < len(children); j++ {
			if children[j].Name < children[i].Name {
				children[i], children[j] = children[j], children[i]
			}
		}
	}
}

func flattenTree(root *TreeNode) []FlatTreeItem {
	var items []FlatTreeItem
	flattenTreeRecursive(root, &items, true)
	return items
}

func flattenTreeRecursive(node *TreeNode, items *[]FlatTreeItem, isLast bool) {
	// Skip root node itself (it's just a container)
	if node.Depth > 0 {
		item := FlatTreeItem{
			Node:   node,
			Index:  len(*items),
			IsLast: isLast,
		}
		*items = append(*items, item)
	}

	// If it's an expanded directory, add its children
	if node.IsDirectory && node.IsExpanded && len(node.Children) > 0 {
		// Sort children alphabetically
		sortChildren(node.Children)

		for i, child := range node.Children {
			isLastChild := i == len(node.Children)-1
			flattenTreeRecursive(child, items, isLastChild)
		}
	}
}


func truncateSourceName(name string, maxLen int) string {
	// Show only the last part of the path (filename)
	parts := strings.Split(name, "/")
	if len(parts) > 1 {
		// Get the last non-empty part
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != "" {
				lastPart := parts[i]
				if len(lastPart) > maxLen {
					return "…" + lastPart[len(lastPart)-maxLen+1:]
				}
				return lastPart
			}
		}
	}

	// If no slashes, just truncate if needed
	if len(name) > maxLen {
		return "…" + name[len(name)-maxLen+1:]
	}
	return name
}

func wrapText(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}

	var result []string
	for len(text) > width {
		// Find a good break point (prefer breaking at slash)
		breakPoint := width
		lastSlash := strings.LastIndex(text[:width], "/")
		if lastSlash > width/2 {
			breakPoint = lastSlash + 1
		}

		result = append(result, text[:breakPoint])
		text = text[breakPoint:]
	}

	if len(text) > 0 {
		result = append(result, text)
	}

	return result
}

func buildAttribution(width int) string {
	// Made with ♥ and Claude Code by Mohit
	// With OSC 8 hyperlink for Mohit
	mohitLink := "\033]8;;https://mohitkarekar.com\033\\Mohit\033]8;;\033\\"
	simpleAttribution := "Made with ♥ and Claude Code by " + mohitLink

	style := lipgloss.NewStyle().Faint(true).Align(lipgloss.Right)
	return style.Render(simpleAttribution)
}

func (m *model) updateSearchMatches() {
	m.searchMatches = nil
	m.currentMatch = 0

	if m.searchQuery == "" || m.selectedIdx >= len(m.sources) {
		return
	}

	code := m.sources[m.selectedIdx].Code
	lines := strings.Split(code, "\n")

	for i, line := range lines {
		if strings.Contains(strings.ToLower(line), strings.ToLower(m.searchQuery)) {
			m.searchMatches = append(m.searchMatches, i)
		}
	}

	// Jump to first match
	if len(m.searchMatches) > 0 {
		m.codeViewport = m.searchMatches[0]
	}
}

func highlightJavaScript(code string) string {
	lexer := lexers.Get("javascript")
	if lexer == nil {
		return code
	}

	style := styles.Get("monokai")
	if style == nil {
		return code
	}

	formatter := formatters.Get("terminal256")
	if formatter == nil {
		return code
	}

	// Tokenize the code
	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return code
	}

	var buf strings.Builder
	err = formatter.Format(&buf, style, iterator)
	if err != nil {
		return code
	}

	return buf.String()
}

func (m model) getSourceIdxFromTreePos(treePos int) int {
	if treePos < 0 || treePos >= len(m.flatTree) {
		return -1
	}
	node := m.flatTree[treePos].Node
	if node != nil && !node.IsDirectory {
		return node.SourceIdx
	}
	return -1
}

func (m model) getTreePosFromSourceIdx(sourceIdx int) int {
	for i, item := range m.flatTree {
		if item.Node != nil && !item.Node.IsDirectory && item.Node.SourceIdx == sourceIdx {
			return i
		}
	}
	return -1
}

func (m model) findNodeInFlatTree(node *TreeNode) int {
	for i, item := range m.flatTree {
		if item.Node == node {
			return i
		}
	}
	return -1
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		}

		if m.state == "view" {
			if m.searchMode {
				switch msg.Type {
				case tea.KeyEscape:
					m.searchMode = false
					m.searchQuery = ""
					m.searchMatches = nil
					m.currentMatch = 0
				case tea.KeyEnter:
					// Go to next match
					if len(m.searchMatches) > 0 {
						m.currentMatch = (m.currentMatch + 1) % len(m.searchMatches)
						m.codeViewport = m.searchMatches[m.currentMatch]
					}
				case tea.KeyBackspace, tea.KeyCtrlH:
					if len(m.searchQuery) > 0 {
						m.searchQuery = m.searchQuery[:len(m.searchQuery)-1]
						m.updateSearchMatches()
					}
				case tea.KeyRunes:
					m.searchQuery += string(msg.Runes)
					m.updateSearchMatches()
				}
			} else {
				switch msg.String() {
				case "q":
					return m, tea.Quit
				case "m":
					// Toggle mouse mode
					m.mouseMode = !m.mouseMode
				case "/":
					m.searchMode = true
					m.searchQuery = ""
					m.searchMatches = nil
					m.currentMatch = 0
				case "up":
					// Navigate up through tree, only selecting files
					for m.selectedTreePos > 0 {
						m.selectedTreePos--
						sourceIdx := m.getSourceIdxFromTreePos(m.selectedTreePos)
						if sourceIdx >= 0 {
							m.selectedIdx = sourceIdx
							m.codeViewport = 0
							break
						}
					}
				case "down":
					// Navigate down through tree, only selecting files
					for m.selectedTreePos < len(m.flatTree)-1 {
						m.selectedTreePos++
						sourceIdx := m.getSourceIdxFromTreePos(m.selectedTreePos)
						if sourceIdx >= 0 {
							m.selectedIdx = sourceIdx
							m.codeViewport = 0
							break
						}
					}
				case "right":
					// Expand directory or select file
					if m.selectedTreePos < len(m.flatTree) {
						node := m.flatTree[m.selectedTreePos].Node
						if node.IsDirectory && !node.IsExpanded {
							node.IsExpanded = true
							m.flatTree = flattenTree(m.fileTree)
							m.selectedTreePos = m.findNodeInFlatTree(node)
						}
					}
				case "left":
					// Collapse directory or navigate to parent
					if m.selectedTreePos < len(m.flatTree) {
						node := m.flatTree[m.selectedTreePos].Node
						if node.IsDirectory && node.IsExpanded {
							node.IsExpanded = false
							m.flatTree = flattenTree(m.fileTree)
							m.selectedTreePos = m.findNodeInFlatTree(node)
						} else if node.Parent != nil && node.Parent.Parent != nil {
							// Navigate to parent directory
							for i := m.selectedTreePos - 1; i >= 0; i-- {
								if m.flatTree[i].Node == node.Parent {
									m.selectedTreePos = i
									break
								}
							}
						}
					}
				}
			}
		}

	case fetchedSourcesMsg:
		if msg.err != nil {
			m.error = msg.err.Error()
			m.state = "error"
		} else {
			m.state = "view"
			m.sources = msg.sources
			// Build file tree from sources
			m.fileTree = buildFileTree(msg.sources)
			// Flatten tree for display
			m.flatTree = flattenTree(m.fileTree)
			// Find first file for initial selection
			m.selectedTreePos = 0
			for i, item := range m.flatTree {
				if !item.Node.IsDirectory {
					m.selectedTreePos = i
					m.selectedIdx = item.Node.SourceIdx
					break
				}
			}
		}
	case tea.MouseMsg:
		if m.state == "view" && m.mouseMode {
			switch msg.Type {
			case tea.MouseWheelUp:
				listWidth := 34
				if msg.X < listWidth {
					// Mouse wheel on left list panel
					if m.listViewport > 0 {
						m.listViewport--
					}
				} else {
					// Mouse wheel on right code panel - scroll 3 lines
					scrollLines := 3
					if m.codeViewport > scrollLines {
						m.codeViewport -= scrollLines
					} else if m.codeViewport > 0 {
						m.codeViewport = 0
					}
				}
			case tea.MouseWheelDown:
				listWidth := 34
				if msg.X < listWidth {
					// Mouse wheel on left list panel
					availableHeight := m.height - 5
					if m.listViewport < len(m.flatTree)-availableHeight {
						m.listViewport++
					}
				} else {
					// Mouse wheel on right code panel - scroll 3 lines
					if m.selectedIdx < len(m.sources) {
						code := m.sources[m.selectedIdx].Code
						lines := strings.Split(code, "\n")
						availableHeight := m.height - 3
						visibleLines := availableHeight - 2
						maxViewport := len(lines) - visibleLines
						if maxViewport < 0 {
							maxViewport = 0
						}
						scrollLines := 3
						if m.codeViewport+scrollLines < maxViewport {
							m.codeViewport += scrollLines
						} else if m.codeViewport < maxViewport {
							m.codeViewport = maxViewport
						}
					}
				}
			case tea.MouseLeft:
				// Handle left click on list items
				// The list is on the left side, approximately 34 characters wide (30 + borders/padding)
				listWidth := 34
				if msg.X < listWidth {
					// Click is in the list area
					// Account for: header (1) + border top (1) + padding top (1) = 3 lines before content
					contentStartLine := 3
					clickedLine := msg.Y - contentStartLine + 1
					// Account for scrolling with listViewport
					treePos := m.listViewport + clickedLine
					if treePos >= 0 && treePos < len(m.flatTree) {
						node := m.flatTree[treePos].Node
						if node.IsDirectory {
							// Toggle expansion
							node.IsExpanded = !node.IsExpanded
							m.flatTree = flattenTree(m.fileTree)
							m.selectedTreePos = m.findNodeInFlatTree(node)
						} else {
							// Select file
							m.selectedTreePos = treePos
							m.selectedIdx = node.SourceIdx
							m.codeViewport = 0
						}
					}
				}
			}
		}
	}

	return m, nil
}

func (m model) View() string {
	switch m.state {
	case "loading":
		return "Loading sources...\n"
	case "error":
		return fmt.Sprintf("Error: %s\n\nPress ctrl+c to exit\n", m.error)
	case "view":
		return m.mainView()
	default:
		return ""
	}
}

func (m model) mainView() string {
	if len(m.sources) == 0 {
		return "No sources found"
	}

	// Guard against uninitialized dimensions
	width := m.width
	height := m.height
	if width == 0 {
		width = 80
	}
	if height == 0 {
		height = 24
	}

	// Calculate heights (reserve space for header and footer)
	availableHeight := height - 5
	listWidth := 30
	codeWidth := width - listWidth - 4 // Account for borders and padding

	// Build header with unmap label and URL
	unmapLabel := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("6")).
		Render("unmap")
	urlDisplay := fmt.Sprintf("URL: %s", m.url)
	headerContent := unmapLabel + " • " + urlDisplay
	urlHeader := lipgloss.NewStyle().
		Width(width).
		Padding(0, 1).
		Background(lipgloss.Color("8")).
		Render(headerContent)

	listStyle := lipgloss.NewStyle().
		Width(listWidth).
		Height(availableHeight).
		Border(lipgloss.RoundedBorder()).
		Padding(1)

	codeStyle := lipgloss.NewStyle().
		Width(codeWidth).
		Height(availableHeight).
		Border(lipgloss.RoundedBorder()).
		Padding(1)

	// Build tree view with scrolling support
	list := ""
	smallTextStyle := lipgloss.NewStyle().Faint(true)
	maxNameWidth := listWidth - 4 // Account for border and padding

	// Calculate visible range based on viewport
	visibleLines := availableHeight - 2 // Account for border and padding
	startIdx := m.listViewport
	endIdx := startIdx + visibleLines
	if endIdx > len(m.flatTree) {
		endIdx = len(m.flatTree)
	}

	// Build only the visible items
	for i := startIdx; i < endIdx; i++ {
		item := m.flatTree[i]
		node := item.Node

		// Build tree structure with vertical lines
		var displayLine string
		if node.Depth == 0 {
			// Root level items have no prefix
			displayLine = node.Name
		} else {
			// Build prefix with vertical lines for parent connections
			var prefix string
			levels := make([]bool, node.Depth)

			// Walk up to root to determine which parents have siblings
			temp := node
			for d := node.Depth - 1; d > 0; d-- {
				if temp.Parent != nil && temp.Parent.Parent != nil {
					// Check if this parent has a next sibling
					if temp.Parent.Parent.Children != nil {
						lastChild := temp.Parent.Parent.Children[len(temp.Parent.Parent.Children)-1]
						levels[d] = temp.Parent != lastChild
					}
				}
				temp = temp.Parent
			}

			// Build the prefix with vertical lines
			for d := 1; d < node.Depth; d++ {
				if levels[d] {
					prefix += "│ "
				} else {
					prefix += "  "
				}
			}

			displayLine = prefix + node.Name
		}

		displayName := displayLine
		// Account for actual display width when truncating
		if len(displayName) > maxNameWidth {
			displayName = displayName[:maxNameWidth-1] + "…"
		}

		// Highlight selected item
		if m.selectedTreePos == i {
			item := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")).Render(displayName)
			list += item + "\n"
		} else {
			item := smallTextStyle.Render(displayName)
			list += item + "\n"
		}
	}

	// Fill remaining space if list doesn't fill the entire height
	for i := endIdx - startIdx; i < visibleLines; i++ {
		list += "\n"
	}

	// Build code view
	code := "No code available"
	if m.selectedIdx < len(m.sources) {
		sourceName := m.sources[m.selectedIdx].Name
		fullCode := m.sources[m.selectedIdx].Code
		originalLines := strings.Split(fullCode, "\n")

		// Prepend source name as header
		sourceHeader := lipgloss.NewStyle().Faint(true).Render(sourceName)
		codeContent := "No code available"

		// Apply syntax highlighting
		highlightedCode := highlightJavaScript(fullCode)
		lines := strings.Split(highlightedCode, "\n")

		// Calculate how many lines we can show (account for source name header)
		visibleLines := availableHeight - 3 // Account for border, padding, and source name
		start := m.codeViewport
		end := start + visibleLines
		if end > len(lines) {
			end = len(lines)
		}
		if start < len(lines) {
			displayLines := lines[start:end]

			// Highlight search matches by replacing text in the line
			if m.searchMode && m.searchQuery != "" {
				highlightStyle := lipgloss.NewStyle().Background(lipgloss.Color("226")).Foreground(lipgloss.Color("0"))
				for j := range displayLines {
					actualLineIdx := start + j
					if actualLineIdx < len(originalLines) {
						originalLine := originalLines[actualLineIdx]
						// Replace search query with highlighted version in the syntax-highlighted line
						if strings.Contains(strings.ToLower(originalLine), strings.ToLower(m.searchQuery)) {
							// Find and replace the search term (case-insensitive)
							lowerLine := strings.ToLower(displayLines[j])
							lowerQuery := strings.ToLower(m.searchQuery)
							idx := strings.Index(lowerLine, lowerQuery)
							if idx >= 0 {
								before := displayLines[j][:idx]
								match := displayLines[j][idx : idx+len(m.searchQuery)]
								after := displayLines[j][idx+len(m.searchQuery):]
								displayLines[j] = before + highlightStyle.Render(match) + after
							}
						}
					}
				}
			}

			codeContent = strings.Join(displayLines, "\n")
		}

		code = sourceHeader + "\n" + codeContent
	}

	layout := lipgloss.JoinHorizontal(
		lipgloss.Top,
		listStyle.Render(list),
		codeStyle.Render(code),
	)

	// Build footer with search info and mouse mode indicator
	mouseIndicator := "🖱️"
	if !m.mouseMode {
		mouseIndicator = "✎"
	}
	footerText := fmt.Sprintf("↑/↓: navigate | /: search | m: toggle mouse (%s) | q: quit | ctrl+c: exit", mouseIndicator)
	if m.searchMode {
		matchInfo := ""
		if len(m.searchMatches) > 0 {
			matchInfo = fmt.Sprintf(" (%d/%d)", m.currentMatch+1, len(m.searchMatches))
		} else if m.searchQuery != "" {
			matchInfo = " (no matches)"
		}
		footerText = fmt.Sprintf("Search: %s%s | Enter: next | Esc: close", m.searchQuery, matchInfo)
	}

	footer := lipgloss.NewStyle().Faint(true).Render(footerText)
	attribution := buildAttribution(width)

	return urlHeader + "\n" + layout + "\n" + footer + "\n" + attribution
}

type fetchedSourcesMsg struct {
	sources []Source
	err     error
}

func fetchSourcesMsg(url string, headers map[string]string) tea.Msg {
	sources, err := fetchAndParseSourcemap(url, headers)
	return fetchedSourcesMsg{sources: sources, err: err}
}

func fetchAndParseSourcemap(jsURL string, headers map[string]string) ([]Source, error) {
	// Fetch the JS file
	req, err := http.NewRequest("GET", jsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add default User-Agent if not provided
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	// Add custom headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JS file: %w", err)
	}
	defer resp.Body.Close()

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read JS file: %w", err)
	}

	jsContent := string(content)

	// Find sourcemap URL in JS file
	sourcemapURL := extractSourcemapURL(jsContent)
	if sourcemapURL == "" {
		return nil, fmt.Errorf("no sourcemap URL found in JS file")
	}

	// Resolve relative URLs
	if strings.HasPrefix(sourcemapURL, "data:") {
		return parseDataURLSourcemap(sourcemapURL)
	}

	if !strings.HasPrefix(sourcemapURL, "http") {
		// Make it relative to the JS file
		if strings.HasPrefix(sourcemapURL, "/") {
			sourcemapURL = strings.TrimSuffix(jsURL, "/") + sourcemapURL
		} else {
			sourcemapURL = strings.TrimSuffix(jsURL, "/") + "/" + sourcemapURL
		}
	}

	return fetchSourcemapFromURL(sourcemapURL, headers)
}

func extractSourcemapURL(jsContent string) string {
	// Look for: //# sourceMappingURL=...
	re := regexp.MustCompile(`//# sourceMappingURL=(.+?)(?:\n|$)`)
	matches := re.FindStringSubmatch(jsContent)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func parseDataURLSourcemap(dataURL string) ([]Source, error) {
	// Handle data:application/json;base64,...
	parts := strings.Split(dataURL, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid data URL format")
	}

	data := parts[1]
	var decoded []byte
	var err error

	if strings.Contains(parts[0], "base64") {
		decoded, err = base64.StdEncoding.DecodeString(data)
	} else {
		decoded = []byte(data)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to decode sourcemap: %w", err)
	}

	return parseSourcemapJSON(decoded)
}

func fetchSourcemapFromURL(sourcemapURL string, headers map[string]string) ([]Source, error) {
	req, err := http.NewRequest("GET", sourcemapURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add default User-Agent if not provided
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	// Add custom headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch sourcemap: %w", err)
	}
	defer resp.Body.Close()

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read sourcemap: %w", err)
	}

	// Check if we got a non-200 status
	if resp.StatusCode != http.StatusOK {
		// Return first 200 chars of response for debugging
		preview := string(content)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		// Debug: show which headers were sent
		headerNames := []string{}
		for k := range headers {
			headerNames = append(headerNames, k)
		}
		return nil, fmt.Errorf("HTTP %d (sent headers: %v): %s", resp.StatusCode, headerNames, preview)
	}

	return parseSourcemapJSON(content)
}

func parseSourcemapJSON(data []byte) ([]Source, error) {
	var sm struct {
		Sources []string `json:"sources"`
		Content []string `json:"sourcesContent"`
	}

	err := json.Unmarshal(data, &sm)
	if err != nil {
		// Show first 300 chars of response for debugging
		preview := string(data)
		if len(preview) > 300 {
			preview = preview[:300]
		}
		return nil, fmt.Errorf("failed to parse sourcemap JSON: %w\nResponse: %s", err, preview)
	}

	var sources []Source

	// Try to use sourcesContent first
	if len(sm.Content) > 0 {
		for i, src := range sm.Sources {
			code := ""
			if i < len(sm.Content) && sm.Content[i] != "" {
				code = sm.Content[i]
			}
			sources = append(sources, Source{Name: src, Code: code})
		}
	} else {
		// If no sourcesContent, just create entries with source names
		for _, src := range sm.Sources {
			sources = append(sources, Source{Name: src, Code: "(Source code not available)"})
		}
	}

	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources found in sourcemap")
	}

	return sources, nil
}

func main() {
	// Custom flag parsing to allow flags after positional args
	var url string
	var headers map[string]string

	// Look for URL (first non-flag argument)
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: unmap <url> [-H \"Authorization: Bearer token\"] [--sourcemap <sourcemap-url>]\n")
		fmt.Fprintf(os.Stderr, "Example: unmap https://example.com/app.js -H \"Authorization: Bearer token123\"\n")
		fmt.Fprintf(os.Stderr, "Example: unmap https://example.com/app.js -H \"Authorization: Bearer $(get_token)\" --sourcemap https://example.com/app.js.map\n")
		os.Exit(1)
	}

	url = os.Args[1]
	headers = make(map[string]string)
	sourcemapURL := ""

	// Parse remaining arguments for -H and --sourcemap flags
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "-H" && i+1 < len(os.Args) {
			headerValue := os.Args[i+1]
			// Parse "HeaderName: HeaderValue" format
			parts := strings.SplitN(headerValue, ":", 2)
			if len(parts) == 2 {
				headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
			i++ // Skip next argument since we consumed it
		} else if os.Args[i] == "--sourcemap" && i+1 < len(os.Args) {
			sourcemapURL = os.Args[i+1]
			i++ // Skip next argument since we consumed it
		}
	}

	opts := []tea.ProgramOption{
		tea.WithMouseCellMotion(),
		tea.WithAltScreen(),
	}
	p := tea.NewProgram(initialModel(url, sourcemapURL, headers), opts...)
	if err := p.Start(); err != nil {
		fmt.Printf("Alas, there's been an error: %v\n", err)
		os.Exit(1)
	}
}