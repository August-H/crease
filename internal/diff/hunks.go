package diff

import (
	"fmt"
	"strings"
)

func emitHunks(out *strings.Builder, a, b []string, changes []edit) {
	delta := 0
	for i := 0; i < len(changes); {
		j := i + 1
		for j < len(changes) && changes[j].start <= changes[j-1].end+6 {
			j++
		}
		start, end := max(0, changes[i].start-3), min(len(a), changes[j-1].end+3)
		newStart, newCount := start+delta, end-start
		for _, e := range changes[i:j] {
			newCount += len(e.lines) - (e.end - e.start)
		}
		oldLine, newLine := start, newStart
		if end > start {
			oldLine++
		}
		if newCount > 0 {
			newLine++
		}
		fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", oldLine, end-start, newLine, newCount)
		pos := start
		for _, e := range changes[i:j] {
			for _, line := range a[pos:e.start] {
				emit(out, " ", line)
			}
			for _, line := range a[e.start:e.end] {
				emit(out, "-", line)
			}
			for _, line := range e.lines {
				emit(out, "+", line)
			}
			delta += len(e.lines) - (e.end - e.start)
			pos = e.end
		}
		for _, line := range a[pos:end] {
			emit(out, " ", line)
		}
		i = j
	}
}
