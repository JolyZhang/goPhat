package phatdb

import (
	"fmt"
	"os"
	"strings"
)

func SplitOnSlash(r rune) bool {
	return r == '/'
}

type StatNode struct {
	Version     uint64 // File version
	CVersion    uint64 // Children version
	NumChildren uint64 // Number of children
}

func (s *StatNode) GoString() string {
	return fmt.Sprintf("<SN V=%d CV=%d NC=%d>", s.Version, s.CVersion, s.NumChildren)
}

type DataNode struct {
	Value string
	Stats *StatNode
}

func (d *DataNode) GoString() string {
	return fmt.Sprintf("<DN V=%#v Stats=%#v>", d.Value, d.Stats)
}

type FileNode struct {
	//Parent   *FileNode
	Children map[string]*FileNode
	Data     *DataNode
}

func (f *FileNode) GoString() string {
	return fmt.Sprintf("<FN Children=%#v Data=%#v>", f.Children, f.Data)
}

func GetNodePath(path string) []string {
	parts := strings.FieldsFunc(path, SplitOnSlash)
	return parts
}

func traverseToNode(root *FileNode, parts []string, createMissing bool) (*FileNode, error) {
	temp := root
	// Walk along the path to find our node
	for _, part := range parts {
		if _, exists := temp.Children[part]; !exists {
			if !createMissing {
				return nil, os.ErrNotExist
			}
			// Create any missing nodes along the way
			temp.Children[part] = &FileNode{}
			//temp.Children[part].Parent = temp
			temp = temp.Children[part]
			temp.Children = make(map[string]*FileNode)
			temp.Data = &DataNode{}
			temp.Data.Stats = &StatNode{}
		} else {
			temp = temp.Children[part]
		}
	}
	return temp, nil
}

func createNode(root *FileNode, path string, val string) (*DataNode, error) {
	n, _ := traverseToNode(root, GetNodePath(path), true)
	if n.Data.Stats.Version != 0 {
		return nil, os.ErrExist
	}
	_setNode(n, val)
	return n.Data, nil
}

func deleteNode(root *FileNode, path string) (*StatNode, error) {
	parts := GetNodePath(path)
	n, err := traverseToNode(root, parts, false)
	if err != nil {
		return nil, err
	}
	//p := n.Parent
	p, err := traverseToNode(root, parts[:len(parts)-1], false)
	if err != nil {
		return nil, err
	}
	delete(p.Children, parts[len(parts)-1])
	return n.Data.Stats, nil
}

func existsNode(root *FileNode, path string) (bool, error) {
	n, err := traverseToNode(root, GetNodePath(path), false)
	// If the error is that the file does/doesn't exist, that's no issue
	// NOTE: os.IsExist may also be reasonable here in the future
	if os.IsNotExist(err) {
		return n != nil, nil
	}
	return n != nil, err
}

func getChildren(root *FileNode, path string) ([]string, error) {
	n, err := traverseToNode(root, GetNodePath(path), false)
	if err != nil {
		return nil, err
	}
	var keys []string
	for k := range n.Children {
		keys = append(keys, k)
	}
	return keys, nil
}

func getNode(root *FileNode, path string) (*DataNode, error) {
	n, err := traverseToNode(root, GetNodePath(path), false)
	if err != nil {
		return nil, err
	}
	return n.Data, err
}

func setNode(root *FileNode, path string, val string) (*DataNode, error) {
	n, err := traverseToNode(root, GetNodePath(path), false)
	if err != nil {
		return nil, err
	}
	_setNode(n, val)
	return n.Data, nil
}

func _setNode(n *FileNode, val string) {
	n.Data.Value = val
	n.Data.Stats.Version += 1
}

func hashNode(root *FileNode) string {
	return fmt.Sprintf("%#v", root)
}
