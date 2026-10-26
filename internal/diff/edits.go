package diff

import "errors"

// Myers' frontier search costs little for small edits in large files. Trimming
// equal ends also avoids allocating a frontier proportional to the whole file.
// The search has explicit work and trace bounds for unrelated inputs.
func edits(a, b []string) ([]edit, error) {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	aa, bb := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	n, m := len(aa), len(bb)
	if n == 0 && m == 0 {
		return nil, nil
	}
	if n == 0 || m == 0 {
		return []edit{{start: prefix, end: prefix + n, lines: bb}}, nil
	}
	limit := min(n+m, 2048)
	trace := make([][]int, 0, limit+1)
	work := 0
	get := func(v []int, k int) int {
		d := (len(v) - 1) / 2
		if k < -d || k > d {
			return -1
		}
		return v[k+d]
	}
	for d := 0; d <= limit; d++ {
		v := make([]int, 2*d+1)
		for k := -d; k <= d; k += 2 {
			x := 0
			if d > 0 {
				prev := trace[d-1]
				if k == -d || (k != d && get(prev, k-1) < get(prev, k+1)) {
					x = get(prev, k+1)
				} else {
					x = get(prev, k-1) + 1
				}
			}
			y := x - k
			for x < n && y < m && aa[x] == bb[y] {
				x++
				y++
				work++
			}
			work++
			if work > 16_000_000 {
				return nil, errors.New("text comparison exceeds work limit")
			}
			v[k+d] = x
			if x >= n && y >= m {
				trace = append(trace, v)
				var reverse []byte
				for step := d; step > 0; step-- {
					k := x - y
					prev := trace[step-1]
					pk := k - 1
					if k == -step || (k != step && get(prev, k-1) < get(prev, k+1)) {
						pk = k + 1
					}
					px := get(prev, pk)
					py := px - pk
					for x > px && y > py {
						reverse = append(reverse, '=')
						x--
						y--
					}
					if x == px {
						reverse = append(reverse, '+')
						y--
					} else {
						reverse = append(reverse, '-')
						x--
					}
				}
				for x > 0 && y > 0 {
					reverse = append(reverse, '=')
					x--
					y--
				}
				var out []edit
				i, j := prefix, prefix
				var current *edit
				flush := func() {
					if current != nil {
						out = append(out, *current)
						current = nil
					}
				}
				for z := len(reverse) - 1; z >= 0; z-- {
					op := reverse[z]
					if op == '=' {
						flush()
						i++
						j++
						continue
					}
					if current == nil {
						current = &edit{start: i, end: i}
					}
					if op == '+' {
						current.lines = append(current.lines, b[j])
						j++
					} else {
						i++
						current.end = i
					}
				}
				flush()
				return out, nil
			}
		}
		trace = append(trace, v)
	}
	return nil, errors.New("text comparison exceeds edit-distance limit")
}
