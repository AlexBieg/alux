package graphview

import (
	"context"
	"io/fs"

	"github.com/goccy/go-graphviz"
)

type noFiles struct{}

func (noFiles) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// Graphviz otherwise reads the host disk for images and shape files; no layout here needs a file.
func init() { graphviz.SetFileSystem(noFiles{}) }

// FitPicture draws d within width as cells, each mapped to the node it belongs to: clean dot then routed TD in each direction, a merged LR drawing as the last resort.
func FitPicture(ctx context.Context, d Diagram, width int) (Picture, bool) {
	var merged []Placed
	for _, flow := range []Flow{d.Flow, 1 - d.Flow} {
		d.Flow = flow
		for _, routed := range []bool{false, true} {
			if routed && flow != TD {
				continue
			}
			p, err := layout(ctx, d, routed)
			if err != nil || p.W > width {
				continue
			}
			if p.Clean() {
				return Draw(p), true
			}
			if flow == LR && p.clearOfBoxes() {
				merged = append(merged, p)
			}
		}
	}
	if len(merged) > 0 {
		return Draw(merged[0]), true
	}
	return Picture{}, false
}
