// Command mainprog is a program built on the old API: errors are handled as main handles them.
package main

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

func main() {
	ctx := context.Background()
	store := agent.NewMemStore()
	a := agent.New(agent.NewScriptedModel(agent.TextTurn("hi")), store).WithMaxTurns(5)
	out, err := a.Run(ctx, "r1", "hello")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Text())
	fmt.Println(answer(ctx, a))
}

func answer(ctx context.Context, a *agent.Agent) (agent.Message, error) {
	return a.RunSaga(ctx, "r2", "book it")
}

func build(m agent.Model) (*agent.Agent, error) {
	sub := agent.SubAgent("helper", "helps", agent.New(m, agent.NewMemStore()))
	return agent.New(m, agent.NewMemStore(), sub), nil
}
