// Package graphview draws a diagram into terminal cells that map back to their nodes.
package graphview

type Flow int

const (
	TD Flow = iota
	LR
)

type Node struct {
	Lines  []string
	Header bool
}

// Edge joins nodes by index. Tail and Head are drawn beside the edge's two ends.
type Edge struct {
	From, To   int
	Label      string
	Tail, Head string
	Dashed     bool
	NoArrow    bool
}

type Diagram struct {
	Flow  Flow
	Nodes []Node
	Edges []Edge
}
