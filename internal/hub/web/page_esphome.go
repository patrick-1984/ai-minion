package web

import "net/http"

const pathESPHome = "/workers/esphome"

type esphomePage struct {
	Install FoldBlock
}

func esphomeData(_ *http.Request, _ OpCaller, _ pageUser) (any, error) {
	var p esphomePage
	if tag := installerTag(); tag != "" {
		p.Install = foldBlock("esp32-install", "Copy the flash command", deviceInstallerLine(tag, "esp32.sh", "--port /dev/ttyUSB0"))
	}
	return p, nil
}

func init() {
	RegisterPage(Page{Pattern: pathESPHome, Title: "ESP32 and ESPHome", Template: "esphome.html", Data: userPageData(esphomeData)})
}
