# The portable C core of the thin Worker, compiled from ../../../core (shared with Linux/OpenWrt).
set(core ../../../core)
idf_component_register(
    SRCS ${core}/mt_worker.c ${core}/mt_ws.c ${core}/mt_proto.c ${core}/mt_crypto.c ${core}/mt_enc.c
         ${core}/mt_json.c ${core}/mt_vt.c ${core}/mt_keys.c
         ${core}/third_party/monocypher/monocypher.c ${core}/third_party/monocypher/monocypher-ed25519.c
    INCLUDE_DIRS ${core}
    REQUIRES mbedtls)
# The core is plain C99 and must stay warning-free here too.
target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Wno-unused-parameter)
# The handshake borrows the stream frame pool for its JSON tokens (frame_cap x max_streams in app_main.c).
