package sieve

import "strings"

// Diff returns a minimal line diff of a → b with " ", "-", "+" prefixes.
// Scripts are small, so plain LCS is fine.
func Diff(a, b string) []string {
	x := strings.Split(strings.TrimRight(a, "\n"), "\n")
	y := strings.Split(strings.TrimRight(b, "\n"), "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, "  "+x[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "- "+x[i])
			i++
		default:
			out = append(out, "+ "+y[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+x[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+y[j])
	}
	return out
}
