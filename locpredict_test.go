package main

import (
	"strings"
	"testing"
)

func TestBuildFileMap_Go(t *testing.T) {
	content := `package main

import (
	"fmt"
	"os"
)

type Server struct {
	Port int
}

func NewServer(port int) *Server {
	return &Server{Port: port}
}

func (s *Server) Start() error {
	fmt.Println("starting")
	return nil
}

const maxRetries = 3
`
	fm := BuildFileMap(content, "go")
	if fm == nil {
		t.Fatal("expected non-nil file map")
	}

	if len(fm.Symbols) < 3 {
		t.Fatalf("expected at least 3 symbols, got %d: %+v", len(fm.Symbols), fm.Symbols)
	}

	found := false
	for _, s := range fm.Symbols {
		if s.Kind == "import" {
			found = true
			if s.Line != 3 {
				t.Errorf("import should be on line 3, got %d", s.Line)
			}
		}
	}
	if !found {
		t.Error("expected import symbol")
	}

	found = false
	for _, s := range fm.Symbols {
		if s.Kind == "type" && s.Name == "Server" {
			found = true
		}
	}
	if !found {
		t.Error("expected type Server")
	}

	found = false
	for _, s := range fm.Symbols {
		if s.Kind == "func" && s.Name == "NewServer" {
			found = true
		}
	}
	if !found {
		t.Error("expected func NewServer")
	}
}

func TestBuildFileMap_Rust(t *testing.T) {
	content := `use std::io;

pub struct Config {
    port: u16,
}

impl Config {
    pub fn new(port: u16) -> Self {
        Config { port }
    }
}

pub async fn serve(config: &Config) -> io::Result<()> {
    Ok(())
}
`
	fm := BuildFileMap(content, "rust")
	if fm == nil {
		t.Fatal("expected non-nil file map")
	}

	kinds := map[string]bool{}
	for _, s := range fm.Symbols {
		kinds[s.Kind+":"+s.Name] = true
	}
	if !kinds["type:Config"] {
		t.Error("expected type Config")
	}
	if !kinds["impl:Config"] {
		t.Error("expected impl Config")
	}
	if !kinds["func:serve"] {
		t.Error("expected func serve")
	}
}

func TestBuildFileMap_Python(t *testing.T) {
	content := `import os
from pathlib import Path

class Server:
    def __init__(self, port):
        self.port = port

    def start(self):
        print("starting")

def main():
    s = Server(8080)
    s.start()
`
	fm := BuildFileMap(content, "python")
	if fm == nil {
		t.Fatal("expected non-nil file map")
	}

	kinds := map[string]bool{}
	for _, s := range fm.Symbols {
		kinds[s.Kind+":"+s.Name] = true
	}
	if !kinds["class:Server"] {
		t.Error("expected class Server")
	}
	if !kinds["method:__init__"] {
		t.Error("expected method __init__")
	}
	if !kinds["func:main"] {
		t.Error("expected func main")
	}
}

func TestBuildFileMap_TypeScript(t *testing.T) {
	content := `import { Request, Response } from 'express';

export class Router {
  handle(req: Request): Response {
    return new Response();
  }
}

export function createApp() {
  return new Router();
}

export const handler = async (req: Request) => {
  return new Response();
};
`
	fm := BuildFileMap(content, "node")
	if fm == nil {
		t.Fatal("expected non-nil file map")
	}

	found := false
	for _, s := range fm.Symbols {
		if s.Kind == "class" && s.Name == "Router" {
			found = true
		}
	}
	if !found {
		t.Error("expected class Router")
	}

	found = false
	for _, s := range fm.Symbols {
		if s.Kind == "func" && s.Name == "createApp" {
			found = true
		}
	}
	if !found {
		t.Error("expected func createApp")
	}
}

func TestBuildFileMap_Unknown(t *testing.T) {
	if BuildFileMap("whatever", "haskell") != nil {
		t.Error("expected nil for unknown language")
	}
}

func TestBuildFileMap_Empty(t *testing.T) {
	if BuildFileMap("", "go") != nil {
		t.Error("expected nil for empty content")
	}
}

func TestFormatFileMap(t *testing.T) {
	fm := &FileMap{
		Symbols: []FileSymbol{
			{Kind: "func", Name: "main", Line: 10, End: 20},
			{Kind: "type", Name: "Server", Line: 3, End: 7},
		},
	}
	formatted := FormatFileMap(fm)
	if !strings.Contains(formatted, "[file map]") {
		t.Fatal("expected [file map] header")
	}
	if !strings.Contains(formatted, "func main: L10-20") {
		t.Fatalf("expected func main range, got: %s", formatted)
	}
	if !strings.Contains(formatted, "type Server: L3-7") {
		t.Fatalf("expected type Server range, got: %s", formatted)
	}
}

func TestFormatFileMap_Nil(t *testing.T) {
	if FormatFileMap(nil) != "" {
		t.Error("expected empty for nil")
	}
	if FormatFileMap(&FileMap{}) != "" {
		t.Error("expected empty for empty symbols")
	}
}

func TestFindBlockEnd(t *testing.T) {
	lines := []string{
		"func main() {",
		"    if true {",
		"        return",
		"    }",
		"}",
	}
	end := findBlockEnd(lines, 0)
	if end != 5 {
		t.Errorf("expected end=5, got %d", end)
	}
}

func TestFindPythonBlockEnd(t *testing.T) {
	lines := []string{
		"def foo():",
		"    x = 1",
		"    return x",
		"",
		"def bar():",
	}
	end := findPythonBlockEnd(lines, 0, 0)
	if end != 4 {
		t.Errorf("expected end=4, got %d", end)
	}
}
