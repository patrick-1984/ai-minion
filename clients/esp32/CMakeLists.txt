# Minion thin Worker for ESP32-family chips (ESP-IDF v5.4).
cmake_minimum_required(VERSION 3.16)
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
# Only the components the app needs: keeps the build small and fast.
set(COMPONENTS main)
project(minion-thin)
