// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestSubmitSubscriptionInitialViewSetsAIDigestNavMetadata(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "subscription_submit.go", nil, 0)
	if err != nil {
		t.Fatalf("parse subscription_submit.go: %v", err)
	}

	var submitSubscription *ast.FuncDecl
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if ok && funcDecl.Name.Name == "submitSubscription" {
			submitSubscription = funcDecl
			break
		}
	}
	if submitSubscription == nil {
		t.Fatal("submitSubscription function not found")
	}

	requiredFields := map[string]bool{
		"showAIDigest":  false,
		"countAIDigest": false,
	}

	for _, stmt := range submitSubscription.Body.List {
		ifStmt, ok := stmt.(*ast.IfStmt)
		if ok && rendersTemplate(ifStmt, "add_subscription") {
			break
		}

		call, ok := expressionCall(stmt)
		if !ok {
			continue
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Set" {
			continue
		}

		ident, ok := selector.X.(*ast.Ident)
		if !ok || ident.Name != "v" || len(call.Args) == 0 {
			continue
		}

		field, ok := call.Args[0].(*ast.BasicLit)
		if !ok || field.Kind != token.STRING {
			continue
		}

		switch field.Value {
		case `"showAIDigest"`:
			requiredFields["showAIDigest"] = true
		case `"countAIDigest"`:
			requiredFields["countAIDigest"] = true
		}
	}

	for field, found := range requiredFields {
		if !found {
			t.Fatalf("submitSubscription initial add_subscription view must set %q before rendering validation/error paths", field)
		}
	}
}

func expressionCall(stmt ast.Stmt) (*ast.CallExpr, bool) {
	expressionStatement, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := expressionStatement.X.(*ast.CallExpr)
	return call, ok
}

func rendersTemplate(node ast.Node, templateName string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if found || n == nil {
			return !found
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Render" || len(call.Args) == 0 {
			return true
		}
		argument, ok := call.Args[0].(*ast.BasicLit)
		if ok && argument.Kind == token.STRING && argument.Value == `"`+templateName+`"` {
			found = true
			return false
		}
		return true
	})
	return found
}
