package editor

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// GetAttribute reads HCL from io.Reader, and writes a value to matched
// attribute to io.Writer.
// Note that a filename is used only for an error message.
// If an error occurs, Nothing is written to the output stream.
func GetAttribute(r io.Reader, w io.Writer, filename string, address string) error {
	e := &Editor{
		source: &parser{filename: filename},
		filters: []Filter{
			&attributeGet{address: address},
		},
		sink: &attributeGet{address: address},
	}

	return e.Apply(r, w)
}

// attributeGet is a filter and sink implementation for attribute.
type attributeGet struct {
	address string
}

// Filter reads HCL and writes only matched an attribute at a given address.
func (f *attributeGet) Filter(inFile *hclwrite.File) (*hclwrite.File, error) {
	attr, _, err := findAttribute(inFile.Body(), f.address)
	if err != nil {
		return nil, err
	}

	outFile := hclwrite.NewEmptyFile()
	if attr != nil {
		outFile.Body().SetAttributeRaw(f.address, attr.BuildTokens(nil))
	}

	return outFile, nil
}

// findAttribute returns first matching attribute at a given address.
// If the address does not cantain any dots, find attribute in the body.
// If the address contains dots, the last element is an attribute name,
// and the rest is the address of the block.
// The block is fetched by findLongestMatchingBlocks.
// If the attribute is found, the body containing it is also returned for updating.
func findAttribute(body *hclwrite.Body, address string) (*hclwrite.Attribute, *hclwrite.Body, error) {
	if len(address) == 0 {
		return nil, nil, errors.New("failed to parse address. address is empty")
	}

	a := strings.Split(address, ".")
	if len(a) == 1 {
		// if the address does not cantain any dots, find attribute in the body.
		attr := body.GetAttribute(a[0])
		return attr, body, nil
	}

	// if address contains dots, the last element is an attribute name,
	// and the rest is the address of the block.
	attrName := a[len(a)-1]
	blockAddr := strings.Join(a[:len(a)-1], ".")
	blocks, err := findLongestMatchingBlocks(body, blockAddr)
	if err != nil {
		return nil, nil, err
	}

	if len(blocks) == 0 {
		// not found
		return nil, nil, nil
	}

	// if blocks are matched, check if it has a given attribute name
	for _, b := range blocks {
		attr := b.Body().GetAttribute(attrName)
		if attr != nil {
			// return first matching one.
			return attr, b.Body(), nil
		}
	}

	// not found
	return nil, nil, nil
}

// findLongestMatchingBlocks returns the longest matching blocks at a  given address.
// if the address does not cantain any dots, return all matching blocks by type.
// If the address contains dots, the first element is a block type,
// and the rest is labels or nested block type or composite of them.
// It is ambiguous to find blocks from the address without a schema.
// To distinguish them in address notation requires introducing a strange new
// syntax, which is not user friendly. The address notation is not specified
// in the scope of the HCL specification, So the initial implementation has
// Terraform in mind, but we want to solve it in a schemaless way.
// We prioritize realistic usability over accuracy, we rely on some heuristics
// here to compromise.
// Given the address A.B.C, the user knows if B is a label or a nested block
// type. So if the block matched in either, we should consider it is matched.
// If you had both a label name and a nested block type, the address would be
// A.B.B.C.
// The labels take precedence over nested blocks. This is because if a block
// type is specified, it is assumed that the number of labels in the same block
// type does not really change and only the label name can be changed by the
// user, and we want to give the user room to avoid unintended conflicts.
func findLongestMatchingBlocks(body *hclwrite.Body, address string) ([]*hclwrite.Block, error) {
	if len(address) == 0 {
		return nil, errors.New("failed to parse address. address is empty")
	}

	a := strings.Split(address, ".")
	typeName := a[0]
	blocks := allMatchingBlocksByType(body, typeName)

	if len(a) == 1 {
		// if the address does not cantain any dots,
		// return all matching blocks by type
		return blocks, nil
	}

	matched := []*hclwrite.Block{}
	// if address contains dots, the next element maybe label or nested block.
	for _, b := range blocks {
		labels := b.Labels()
		// consume labels from address
		matchedlabels := longestMatchingLabels(labels, a[1:])
		if len(matchedlabels) < len(labels) {
			// The labels take precedence over nested blocks.
			// If extra labels remain, skip it.
			continue
		}
		if len(matchedlabels) < (len(a)-1) || len(labels) == 0 {
			// if the block has no labels or partially matched ones, find the nested block
			nestedAddr := strings.Join(a[1+len(matchedlabels):], ".")
			nested, err := findLongestMatchingBlocks(b.Body(), nestedAddr)
			if err != nil {
				return nil, err
			}
			matched = append(matched, nested...)
			continue
		}
		// all labels are matched, just add it to matched list.
		matched = append(matched, b)
	}

	return matched, nil
}

// allMatchingBlocksByType returns all matching blocks from the body that have the
// given name or returns an empty list if there is currently no matching block.
// This method is useful when you want to ignore label differences.
func allMatchingBlocksByType(b *hclwrite.Body, typeName string) []*hclwrite.Block {
	matched := []*hclwrite.Block{}
	for _, block := range b.Blocks() {
		if typeName == block.Type() {
			matched = append(matched, block)
		}
	}

	return matched
}

// longestMatchLabels returns a partial labels from the beginning to the
// matching part and returns an empty array if nothing matches.
func longestMatchingLabels(labels []string, prefix []string) []string {
	matched := []string{}
	for i := range prefix {
		if len(labels) <= i {
			return matched
		}
		if prefix[i] != labels[i] {
			return matched
		}
		matched = append(matched, labels[i])
	}
	return matched
}

// Sink reads HCL and writes value of attribute.
func (f *attributeGet) Sink(inFile *hclwrite.File) ([]byte, error) {
	attrName := f.address
	attr := inFile.Body().GetAttribute(attrName)
	if attr == nil {
		return []byte{}, nil
	}

	// treat expr as a string without interpreting its meaning.
	out, err := getAttributeValueAsString(attr)

	if err != nil {
		return []byte{}, err
	}

	return []byte(out + "\n"), nil
}

// getAttributeValueAsString returns a value of Attribute as string.
// There is no way to get value as string directly,
// so we parses tokens of Attribute and build string representation.
func getAttributeValueAsString(attr *hclwrite.Attribute) (string, error) {
	// find TokenEqual
	expr := attr.Expr()
	exprTokens := expr.BuildTokens(nil)
	i := 0
	for i < len(exprTokens) && exprTokens[i].Type != hclsyntax.TokenEqual {
		i++
	}

	if i == len(exprTokens) {
		return "", fmt.Errorf("failed to find TokenEqual: %#v", attr)
	}

	// append tokens until find TokenComment
	var valueTokens hclwrite.Tokens
	for _, t := range exprTokens[(i + 1):] {
		if t.Type == hclsyntax.TokenComment {
			break
		}
		valueTokens = append(valueTokens, t)
	}

	// TokenIdent records SpaceBefore, but we should ignore it here.
	value := strings.TrimSpace(string(valueTokens.Bytes()))

	return value, nil
}
