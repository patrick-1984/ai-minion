// Command forbidden is the negative fixture of headlesscheck: it imports a GUI
// module, so the gate must fail and name this package.
package main

import _ "fyne.io/systray"

func main() {}
