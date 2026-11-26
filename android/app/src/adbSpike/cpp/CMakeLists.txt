# Spike K0: libminion_spake2.so = JNI shim + BoringSSL libcrypto (static), for the adb pairing SPAKE2.
# Built only with -PminionAdbSpike=true (debug builds). Also builds on the host (Linux) for JVM unit tests:
#   cmake -S android/app/src/adbSpike/cpp -B build-host -G Ninja -DMINION_BORINGSSL_DIR=<checkout>
cmake_minimum_required(VERSION 3.22)
project(minion_spake2 C CXX)

# BoringSSL is pinned to one release tag; the archive hash is checked by CMake.
set(MINION_BORINGSSL_TAG "0.20261020.0")
set(MINION_BORINGSSL_SHA256 "fb43aac7fbd323c9dfa1f45fe92a1c01142433c05b23fe34f61d6ba8b13b642e")
set(MINION_BORINGSSL_DIR "" CACHE PATH "Use this BoringSSL checkout of tag ${MINION_BORINGSSL_TAG} instead of downloading")

set(BUILD_TESTING OFF CACHE BOOL "" FORCE)
set(BUILD_SHARED_LIBS OFF CACHE BOOL "" FORCE)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

if(MINION_BORINGSSL_DIR)
  add_subdirectory("${MINION_BORINGSSL_DIR}" boringssl EXCLUDE_FROM_ALL)
else()
  include(FetchContent)
  FetchContent_Declare(boringssl
    URL "https://github.com/google/boringssl/archive/refs/tags/${MINION_BORINGSSL_TAG}.tar.gz"
    URL_HASH "SHA256=${MINION_BORINGSSL_SHA256}"
    EXCLUDE_FROM_ALL)
  FetchContent_MakeAvailable(boringssl)
endif()

add_library(minion_spake2 SHARED spake2_jni.c)
if(NOT ANDROID)
  find_package(JNI REQUIRED)
  target_include_directories(minion_spake2 PRIVATE ${JNI_INCLUDE_DIRS})
endif()
target_link_libraries(minion_spake2 PRIVATE crypto)
target_compile_options(minion_spake2 PRIVATE -Wall -Wextra -Werror -fvisibility=hidden)
# Only the JNI entry points are exported; BoringSSL symbols stay private. 16 KiB pages (Android 15+).
target_link_options(minion_spake2 PRIVATE -Wl,--exclude-libs,ALL -Wl,--gc-sections -Wl,-z,max-page-size=16384)
