package filesystem

import (
	"fmt"
	"strings"
)

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// UnifiedDiff renders a compact unified diff with 3 lines of context.
func UnifiedDiff(name, a, b string) string {
	al, bl := splitLines(a), splitLines(b)
	// LCS table (files are capped at 2 MiB; guard the quadratic cost).
	if len(al)*len(bl) > 4_000_000 {
		return fmt.Sprintf("--- %s\n+++ %s\n[diff omitted: file too large; %d -> %d lines]\n", name, name, len(al), len(bl))
	}
	dp := make([][]int32, len(al)+1)
	for i := range dp {
		dp[i] = make([]int32, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(al) && j < len(bl) {
		switch {
		case al[i] == bl[j]:
			ops = append(ops, diffOp{' ', al[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', al[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', bl[j]})
			j++
		}
	}
	for ; i < len(al); i++ {
		ops = append(ops, diffOp{'-', al[i]})
	}
	for ; j < len(bl); j++ {
		ops = append(ops, diffOp{'+', bl[j]})
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", name, name)
	const ctx = 3
	changed := false
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		changed = true
		start := max(k-ctx, 0)
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			next := end
			for next < len(ops) && ops[next].kind == ' ' {
				next++
			}
			if next >= len(ops) || next-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = next
		}
		fmt.Fprintf(&sb, "@@ around line %d @@\n", lineNo(ops, start))
		for _, o := range ops[start:end] {
			sb.WriteString(string(o.kind) + o.text + "\n")
		}
		k = end
	}
	if !changed {
		return "(no differences)\n"
	}
	return sb.String()
}

// lineNo returns the old-file line number at op index idx.
func lineNo(ops []diffOp, idx int) int {
	n := 1
	for _, o := range ops[:idx] {
		if o.kind != '+' {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffStat counts added and removed lines in a UnifiedDiff result.
func diffStat(diff string) (added, removed int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	return added, removed
}
