// Command missingreq is a fixture of headlesscheck: it imports a module that
// its go.mod does not require, so the read-only module graph is incomplete
// and the gate must fail without adding the requirement.
package main

import _ "example.com/dep"

func main() {}
