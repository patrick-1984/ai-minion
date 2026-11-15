# minion-thin for Linux and OpenWrt.
#   make                      plain sockets (http:// and ws:// Hub URLs)
#   make MBEDTLS=1            TLS through mbedTLS 3.x (https:// and wss://), as on OpenWrt
#   make test                 core unit tests (needs python3 and the repository's golden vectors)
CORE    ?= ../core
REPO    ?= ../..
CC      ?= cc
CFLAGS  ?= -Os
WERROR  ?= -Werror
MT_CFLAGS = $(CFLAGS) -std=c99 -Wall -Wextra $(WERROR) -ffunction-sections -fdata-sections -I$(CORE)
MT_LDFLAGS = $(LDFLAGS) -Wl,--gc-sections
MT_LDLIBS = $(LDLIBS) -lutil
SRCS     = main.c $(CORE)/mt_worker.c $(CORE)/mt_ws.c $(CORE)/mt_proto.c $(CORE)/mt_crypto.c $(CORE)/mt_enc.c \
           $(CORE)/mt_json.c $(CORE)/mt_vt.c $(CORE)/mt_keys.c \
           $(CORE)/third_party/monocypher/monocypher.c $(CORE)/third_party/monocypher/monocypher-ed25519.c
ifeq ($(MBEDTLS),1)
MT_CFLAGS += -DMT_WITH_MBEDTLS
MT_LDLIBS += -lmbedtls -lmbedx509 -lmbedcrypto
endif
BUILD   ?= build

all: $(BUILD)/minion-thin

$(BUILD)/minion-thin: $(SRCS) $(wildcard $(CORE)/*.h)
	@mkdir -p $(BUILD)
	$(CC) $(MT_CFLAGS) -D_GNU_SOURCE $(MT_LDFLAGS) -o $@ $(SRCS) $(MT_LDLIBS)

TESTCORE = $(CORE)/mt_proto.c $(CORE)/mt_crypto.c $(CORE)/mt_enc.c $(CORE)/mt_json.c \
           $(CORE)/third_party/monocypher/monocypher.c $(CORE)/third_party/monocypher/monocypher-ed25519.c

test: $(BUILD)/test_proto $(BUILD)/test_vt $(BUILD)/test_keys
	$(BUILD)/test_proto && $(BUILD)/test_vt && $(BUILD)/test_keys

$(BUILD)/vectors.h: $(CORE)/tests/gen_vectors.py
	@mkdir -p $(BUILD)
	python3 -I $(CORE)/tests/gen_vectors.py $(REPO) $@

$(BUILD)/test_proto: $(CORE)/tests/test_proto.c $(TESTCORE) $(BUILD)/vectors.h
	$(CC) $(MT_CFLAGS) -I$(BUILD) -o $@ $(CORE)/tests/test_proto.c $(TESTCORE)
$(BUILD)/test_vt: $(CORE)/tests/test_vt.c $(CORE)/mt_vt.c $(TESTCORE)
	$(CC) $(MT_CFLAGS) -o $@ $(CORE)/tests/test_vt.c $(CORE)/mt_vt.c $(TESTCORE)
$(BUILD)/test_keys: $(CORE)/tests/test_keys.c $(CORE)/mt_keys.c $(TESTCORE)
	$(CC) $(MT_CFLAGS) -o $@ $(CORE)/tests/test_keys.c $(CORE)/mt_keys.c $(TESTCORE)

clean:
	rm -rf $(BUILD)

.PHONY: all test clean
