// Guardrail-plugin serves the guardrail plugin as a plugin process.
package main

import (
	"log"

	"github.com/majorcontext/harness/examples/guardrail/internal/guard"
	"github.com/majorcontext/harness/plugin"
)

func main() {
	if err := plugin.Serve(guard.Plugin); err != nil {
		log.Fatal(err)
	}
}
