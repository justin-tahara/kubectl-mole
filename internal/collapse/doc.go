// Package collapse merges symptoms that share a cause: a NotReady node
// producing 40 failing pods is one failure entry naming the node, not 40.
// Identical signature + cause across resources collapses to one entry with a
// count and example refs.
package collapse
