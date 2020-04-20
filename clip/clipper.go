package clip

import (
	"GoSlice/data"
	"GoSlice/util"
	"fmt"
	clipper "github.com/ctessum/go.clipper"
)

type Clip interface {
	// GenerateLayerParts partitions the whole layer into several partition parts
	GenerateLayerParts(l data.Layer) (data.PartitionedLayer, bool)
	// InsetLayer should return all new paths generated by insetting all parts of the layer.
	// The result is built the following way: [part][wall][insetNum]
	// * Part is the part in the partitionedLayer
	// * Wall is the wall of the part. The first wall is the outer perimeter
	// * InsetNum is the number of the inset (starting by the outer walls with 0)
	//   and all following are from holes inside of the polygon.
	InsetLayer(layer data.PartitionedLayer, offset util.Micrometer, insetCount int) [][][]data.Paths
	Inset(part data.LayerPart, offset util.Micrometer, insetCount int) [][]data.Paths
	Fill(paths data.Paths, lineWidth util.Micrometer, overlapPercentage int) data.Paths
}

// clipperClip implements Clip using the external clipper library
type clipperClip struct {
}

func NewClip() Clip {
	return clipperClip{}
}

type layerPart struct {
	outline data.Path
	holes   data.Paths
}

func (l layerPart) Outline() data.Path {
	return l.outline
}

func (l layerPart) Holes() data.Paths {
	return l.holes
}

type partitionedLayer struct {
	parts []data.LayerPart
}

func (p partitionedLayer) LayerParts() []data.LayerPart {
	return p.parts
}

func clipperPoint(p util.MicroPoint) *clipper.IntPoint {
	return &clipper.IntPoint{
		X: clipper.CInt(p.X()),
		Y: clipper.CInt(p.Y()),
	}
}

func clipperPaths(p data.Paths) clipper.Paths {
	var result clipper.Paths
	for _, path := range p {
		var newPath clipper.Path
		for _, point := range path {
			newPath = append(newPath, clipperPoint(point))
		}
		result = append(result, newPath)
	}

	return result
}

func microPoint(p *clipper.IntPoint) util.MicroPoint {
	return util.NewMicroPoint(util.Micrometer(p.X), util.Micrometer(p.Y))
}

func microPath(p clipper.Path) data.Path {
	var result data.Path
	for _, point := range p {
		result = append(result, microPoint(point))
	}
	return result
}

func microPaths(p clipper.Paths, simplify bool) data.Paths {
	var result data.Paths

	for _, path := range p {
		microPath := microPath(path)

		if simplify {
			microPath = microPath.Simplify(-1, -1)
		}

		result = append(result, microPath)
	}

	return result
}

func (c clipperClip) GenerateLayerParts(l data.Layer) (data.PartitionedLayer, bool) {
	polyList := clipper.Paths{}
	// convert all polygons to clipper polygons
	for _, layerPolygon := range l.Polygons() {
		var path = clipper.Path{}

		prev := 0
		// convert all points of this polygons
		for j, layerPoint := range layerPolygon {
			// ignore first as the next check would fail otherwise
			if j == 0 {
				path = append(path, clipperPoint(layerPolygon[0]))
				continue
			}

			// filter too near points
			// check this always with the previous point
			if layerPoint.Sub(layerPolygon[prev]).ShorterThan(100) {
				continue
			}

			path = append(path, clipperPoint(layerPoint))
			prev = j
		}

		polyList = append(polyList, path)
	}

	layer := partitionedLayer{}

	clip := clipper.NewClipper(clipper.IoNone)
	clip.AddPaths(polyList, clipper.PtSubject, true)
	resultPolys, ok := clip.Execute2(clipper.CtUnion, clipper.PftEvenOdd, clipper.PftEvenOdd)
	if !ok {
		return nil, false
	}

	polysForNextRound := []*clipper.PolyNode{}

	for _, c := range resultPolys.Childs() {
		polysForNextRound = append(polysForNextRound, c)
	}
	for {
		if polysForNextRound == nil {
			break
		}
		thisRound := polysForNextRound
		polysForNextRound = nil

		for _, p := range thisRound {

			part := layerPart{
				outline: microPath(p.Contour()),
			}
			for _, child := range p.Childs() {
				part.holes = append(part.holes, microPath(child.Contour()))
				for _, c := range child.Childs() {
					polysForNextRound = append(polysForNextRound, c)
				}
			}
			layer.parts = append(layer.parts, &part)
		}
	}
	return layer, true
}

func (c clipperClip) InsetLayer(layer data.PartitionedLayer, offset util.Micrometer, insetCount int) [][][]data.Paths {
	var result [][][]data.Paths
	for _, part := range layer.LayerParts() {
		result = append(result, c.Inset(part, offset, insetCount))
	}

	return result
}

func (c clipperClip) Inset(part data.LayerPart, offset util.Micrometer, insetCount int) [][]data.Paths {
	var insets [][]data.Paths

	o := clipper.NewClipperOffset()

	for insetNr := 0; insetNr < insetCount; insetNr++ {
		// insets for the outline
		o.Clear()
		o.AddPaths(clipperPaths(data.Paths{part.Outline()}), clipper.JtSquare, clipper.EtClosedPolygon)
		o.AddPaths(clipperPaths(part.Holes()), clipper.JtSquare, clipper.EtClosedPolygon)

		o.MiterLimit = 2
		allNewInsets := o.Execute(float64(-int(offset)*insetNr) - float64(offset/2))

		if len(allNewInsets) <= 0 {
			break
		} else {
			for wallNr, wall := range microPaths(allNewInsets, true) {
				if len(insets) <= wallNr {
					insets = append(insets, []data.Paths{})
				}

				// It can happen that clipper generates new walls which the previous insets didn't have
				// for example if it generates a filling polygon in the corners.
				// We add empty paths so that the insetNr is still correct.
				for len(insets[wallNr]) <= insetNr {
					insets[wallNr] = append(insets[wallNr], []data.Path{})
				}

				insets[wallNr][insetNr] = append(insets[wallNr][insetNr], wall)
			}
		}
	}

	return insets
}

func (c clipperClip) Fill(paths data.Paths, lineWidth util.Micrometer, overlapPercentage int) data.Paths {
	min, max := paths.Size()
	cPaths := clipperPaths(paths)
	result := c.getLinearFill(cPaths, min, max, lineWidth, overlapPercentage)
	return microPaths(result, false)
}

func (c clipperClip) getLinearFill(polys clipper.Paths, minScanlines util.MicroPoint, maxScanlines util.MicroPoint, lineWidth util.Micrometer, overlapPercentage int) clipper.Paths {
	cl := clipper.NewClipper(clipper.IoNone)
	co := clipper.NewClipperOffset()
	var result clipper.Paths

	overlap := float32(lineWidth) * (100.0 - float32(overlapPercentage)) / 100.0

	lines := clipper.Paths{}
	numLine := 0
	for x := minScanlines.X(); x <= maxScanlines.X(); x += lineWidth {
		// switch line direction based on even / odd
		if numLine%2 == 1 {
			lines = append(lines, clipper.Path{
				&clipper.IntPoint{
					X: clipper.CInt(x),
					Y: clipper.CInt(maxScanlines.Y()),
				},
				&clipper.IntPoint{
					X: clipper.CInt(x),
					Y: clipper.CInt(minScanlines.Y()),
				},
			})
		} else {
			lines = append(lines, clipper.Path{
				&clipper.IntPoint{
					X: clipper.CInt(x),
					Y: clipper.CInt(minScanlines.Y()),
				},
				&clipper.IntPoint{
					X: clipper.CInt(x),
					Y: clipper.CInt(maxScanlines.Y()),
				},
			})
		}
		numLine++
	}

	for _, path := range polys {
		cl.Clear()
		co.Clear()
		co.AddPath(path, clipper.JtSquare, clipper.EtClosedPolygon)
		co.MiterLimit = 2
		newInsets := co.Execute(float64(-overlap))

		cl.AddPaths(newInsets, clipper.PtClip, true)
		cl.AddPaths(lines, clipper.PtSubject, false)

		tree, ok := cl.Execute2(clipper.CtIntersection, clipper.PftEvenOdd, clipper.PftEvenOdd)
		if !ok {
			fmt.Println("getLinearFill failed")
			return nil
		}

		for _, c := range tree.Childs() {
			result = append(result, c.Contour())
		}
	}

	return result
}
