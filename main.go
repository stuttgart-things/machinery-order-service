// machinery-order-service takes orders for machinery resources, renders them
// through claim-machinery-api and delivers them to git as pull requests
// (stuttgart-things/stuttgart-things#4297).
package main

import "github.com/stuttgart-things/machinery-order-service/cmd"

func main() { cmd.Execute() }
