package main

import (
	"go/ast"
	"go/types"
	"strconv"
)

// The rename rule: names that moved package.
//
//   - Package mcp is mcptools (github.com/bide-ai/bide/mcptools): the import and every
//     qualifier of an unnamed import change.
//   - The scripted model, a test double, moved to package agenttest: ScriptedModel, ScriptedTurn,
//     NewScriptedModel, ToolTurn, TextTurn and ErrorTurn. (Package agent's own tests define the
//     same names in a test file, so their uses inside package agent stay.)

var scripted = map[string]bool{"ScriptedModel": true, "ScriptedTurn": true, "NewScriptedModel": true, "ToolTurn": true, "TextTurn": true, "ErrorTurn": true}

func visitRename(c *Ctx, n ast.Node) {
	switch n := n.(type) {
	case *ast.ImportSpec:
		if p, _ := strconv.Unquote(n.Path.Value); p == mcpPath {
			c.Ed.Replace(n.Path, strconv.Quote(mcptoolsPath))
			c.Count()
		}
	case *ast.SelectorExpr:
		id, ok := n.X.(*ast.Ident)
		if !ok {
			return
		}
		pn, ok := c.Info.Uses[id].(*types.PkgName)
		if !ok {
			return
		}
		switch pn.Imported().Path() {
		case mcpPath:
			if id.Name == "mcp" {
				c.Ed.Replace(id, "mcptools")
				c.Count()
			}
		case agentPath:
			if scripted[n.Sel.Name] && !c.InAgent() {
				c.Ed.Replace(n, c.Q(agenttestPath)+n.Sel.Name)
				c.Count()
			}
		}
	}
}
