// Run the actual price resolver without a provider connection or user account.
package main
import (
 "fmt"
 "github.com/SuperMarioYL/agentfuse/internal/prices"
)
func main() {
 table, err := prices.LoadBundled(); if err != nil { panic(err) }
 fmt.Println("Bundled price snapshot:", prices.SnapshotDate)
 for _, model := range []string{"gpt-4o", "unlisted-demo-model"} {
  entry, found := table.Lookup("openai", model)
  fmt.Printf("openai/%s explicit=%t input_per_1k=%.4f output_per_1k=%.4f\n", model, found, entry.InputUSDPer1K, entry.OutputUSDPer1K)
  fmt.Printf("  configured cost for 1000 input + 100 output tokens: $%.4f\n", table.Cost("openai", model, 1000, 100))
 }
 fmt.Println("Scope: local price-table arithmetic; no provider request or measured spend.")
}
