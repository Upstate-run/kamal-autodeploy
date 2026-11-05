package cli

import (
	"fmt"
	"strings"
)

// unifiedDiff is a small line diff (LCS) with three lines of context, enough to review a
// generated file before it is written.
func unifiedDiff(name string, before, after []byte) string {
	a := splitLines(string(before))
	b := splitLines(string(after))
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			ops = append(ops, op{'+', b[j]})
			j++
		default:
			ops = append(ops, op{'-', a[i]})
			i++
		}
	}
	changed := false
	for _, o := range ops {
		if o.kind != ' ' {
			changed = true
		}
	}
	if !changed {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s (current)\n+++ %s (generated)\n", name, name)
	const ctx = 3
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for d := -ctx; d <= ctx; d++ {
				if k+d >= 0 && k+d < len(ops) {
					keep[k+d] = true
				}
			}
		}
	}
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap {
			out.WriteString("@@ ... @@\n")
			gap = false
		}
		out.WriteByte(o.kind)
		out.WriteString(o.text)
		out.WriteByte('\n')
	}
	return out.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
