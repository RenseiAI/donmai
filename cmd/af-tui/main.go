package main

import (
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/joho/godotenv"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
	"github.com/RenseiAI/agentfactory-tui/internal/app"
)

func main() {
	_ = godotenv.Load(".env.local", ".env")

	defaultURL := "http://localhost:3000"
	if u := os.Getenv("WORKER_API_URL"); u != "" {
		defaultURL = u
	}
	baseURL := flag.String("url", defaultURL, "AgentFactory server URL")
	mock := flag.Bool("mock", false, "Use mock data instead of live API")
	flag.Parse()

	var ds api.DataSource
	if *mock {
		ds = api.NewMockClient()
	} else {
		ds = api.NewClient(*baseURL)
	}

	ctx := &app.Context{
		DataSource: ds,
		BaseURL:    *baseURL,
		UseMock:    *mock,
	}

	model := app.New(ctx)
	p := tea.NewProgram(model)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
