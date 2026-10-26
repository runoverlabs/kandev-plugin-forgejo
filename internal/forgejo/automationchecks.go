package forgejo

import (
	"regexp"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/prautomation"
)

// automationCI maps a combined status. A cancelled or skipped check is not a
// failure: nothing in it asks for a change.
func automationCI(combined rest.CombinedStatus) prautomation.CI {
	ci := prautomation.CI{State: prautomation.CINone}
	if len(combined.Statuses) == 0 {
		return ci
	}
	pending := false
	for _, status := range combined.Statuses {
		switch strings.ToLower(strings.TrimSpace(status.Status)) {
		case "failure", "error":
			name := strings.TrimSpace(status.Context)
			if name == "" {
				name = "check"
			}
			ci.Failing = append(ci.Failing, prautomation.Check{
				Name:  name,
				State: strings.ToLower(strings.TrimSpace(status.Status)),
				URL:   strings.TrimSpace(status.TargetURL),
			})
		case "pending", "running":
			pending = true
		}
	}
	switch {
	case pending:
		ci.State = prautomation.CIPending
	case len(ci.Failing) > 0:
		ci.State = prautomation.CIFailure
	default:
		ci.State = prautomation.CISuccess
	}
	return ci
}

// missingRequiredChecks lists the required context patterns no status on the
// head matches. The instance holds such a pattern pending and refuses the
// merge, so a check that has not reported yet is not a green build even when
// every check that has is.
func missingRequiredChecks(patterns []string, statuses []rest.CommitStatus) []string {
	var missing []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		matcher := contextGlob(pattern)
		found := false
		for _, status := range statuses {
			if matcher != nil && matcher.MatchString(status.Context) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, pattern)
		}
	}
	return missing
}

// contextGlob compiles a required context pattern as the instance does
// (gobwas/glob without separators): "*" matches any run of characters, "/"
// included, "?" one character, "[...]" a class ("[!...]" negated) and
// "{a,b}" either alternative. A pattern it cannot compile matches nothing, so
// auto-merge waits rather than guessing.
func contextGlob(pattern string) *regexp.Regexp {
	expression, ok := globExpression([]rune(pattern))
	if !ok {
		return nil
	}
	compiled, err := regexp.Compile(expression)
	if err != nil {
		return nil
	}
	return compiled
}

// globExpression translates a glob into an anchored regular expression, or
// reports it malformed: an unclosed class or an unbalanced brace.
func globExpression(runes []rune) (string, bool) {
	var b strings.Builder
	b.WriteString("^")
	depth := 0
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			class, end, ok := globClass(runes, i)
			if !ok {
				return "", false
			}
			b.WriteString(class)
			i = end
		case '{':
			depth++
			b.WriteString("(?:")
		case '}':
			if depth == 0 {
				return "", false
			}
			depth--
			b.WriteString(")")
		case ',':
			b.WriteString(globComma(depth))
		case '\\':
			if i+1 < len(runes) {
				i++
				b.WriteString(regexp.QuoteMeta(string(runes[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if depth != 0 {
		return "", false
	}
	b.WriteString("$")
	return b.String(), true
}

// globClass translates the class opening at start and returns the index of
// its closing bracket.
func globClass(runes []rune, start int) (string, int, bool) {
	end := start + 1
	for end < len(runes) && runes[end] != ']' {
		end++
	}
	if end == len(runes) {
		return "", 0, false
	}
	class := string(runes[start+1 : end])
	if strings.HasPrefix(class, "!") {
		class = "^" + class[1:]
	}
	return "[" + strings.ReplaceAll(class, `\`, `\\`) + "]", end, true
}

// globComma separates alternatives inside braces and is literal outside them.
func globComma(depth int) string {
	if depth > 0 {
		return "|"
	}
	return ","
}
