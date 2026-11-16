idf_component_register(
    SRCS app_main.c platform.c cfg.c net.c sessions.c cmds.c
    INCLUDE_DIRS .
    REQUIRES minion_core mbedtls nvs_flash esp_wifi esp_eth esp_netif esp_event esp_timer console
             esp_driver_gpio esp_driver_uart esp_driver_ledc esp_driver_i2c esp_adc lwip esp_hw_support)
target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Wno-unused-parameter)
