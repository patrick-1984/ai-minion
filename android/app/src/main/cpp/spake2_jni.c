// Device control: JNI shim over BoringSSL SPAKE2 (spake25519) for adb Wireless debugging pairing.
// SPDX-License-Identifier: Apache-2.0
//
// The adb pairing protocol (AOSP packages/modules/adb pairing_auth/pairing_auth.cpp) uses BoringSSL
// SPAKE2_CTX with the client as Alice ("adb pair client\0") and the server as Bob ("adb pair server\0").
// The password is the 6-digit code followed by 64 bytes of TLS exporter output. This file exposes the four
// calls the Kotlin side needs. No secret leaves this file except the derived key material, which the caller
// wipes after use. Nothing is logged.

#include <jni.h>
#include <stdint.h>
#include <string.h>

#include <openssl/curve25519.h>
#include <openssl/mem.h>

#define JNI_FN(name) Java_cc_nasz_minion_devicecontrol_Spake2_##name

static void throw_state(JNIEnv *env, const char *msg) {
    jclass c = (*env)->FindClass(env, "java/lang/IllegalStateException");
    if (c != NULL) (*env)->ThrowNew(env, c, msg);
}

// role: 0 = Alice (client), 1 = Bob (server, used by tests only).
JNIEXPORT jlong JNICALL JNI_FN(nativeNew)(JNIEnv *env, jclass clazz, jint role, jbyteArray my_name,
                                          jbyteArray their_name) {
    (void)clazz;
    jsize my_len = (*env)->GetArrayLength(env, my_name);
    jsize their_len = (*env)->GetArrayLength(env, their_name);
    jbyte *my = (*env)->GetByteArrayElements(env, my_name, NULL);
    jbyte *their = (*env)->GetByteArrayElements(env, their_name, NULL);
    SPAKE2_CTX *ctx = NULL;
    if (my != NULL && their != NULL) {
        ctx = SPAKE2_CTX_new(role == 0 ? spake2_role_alice : spake2_role_bob, (const uint8_t *)my,
                             (size_t)my_len, (const uint8_t *)their, (size_t)their_len);
    }
    if (my != NULL) (*env)->ReleaseByteArrayElements(env, my_name, my, JNI_ABORT);
    if (their != NULL) (*env)->ReleaseByteArrayElements(env, their_name, their, JNI_ABORT);
    if (ctx == NULL) throw_state(env, "SPAKE2_CTX_new failed");
    return (jlong)(intptr_t)ctx;
}

JNIEXPORT jbyteArray JNICALL JNI_FN(nativeGenerateMsg)(JNIEnv *env, jclass clazz, jlong handle,
                                                       jbyteArray password) {
    (void)clazz;
    SPAKE2_CTX *ctx = (SPAKE2_CTX *)(intptr_t)handle;
    uint8_t msg[SPAKE2_MAX_MSG_SIZE];
    size_t msg_len = 0;
    jsize pw_len = (*env)->GetArrayLength(env, password);
    jbyte *pw = (*env)->GetByteArrayElements(env, password, NULL);
    if (ctx == NULL || pw == NULL) {
        if (pw != NULL) (*env)->ReleaseByteArrayElements(env, password, pw, JNI_ABORT);
        throw_state(env, "SPAKE2 context or password missing");
        return NULL;
    }
    int ok = SPAKE2_generate_msg(ctx, msg, &msg_len, sizeof(msg), (const uint8_t *)pw, (size_t)pw_len);
    OPENSSL_cleanse(pw, (size_t)pw_len);
    // Mode 0 copies the cleansed buffer back, so a copying JVM also ends up with a wiped array.
    (*env)->ReleaseByteArrayElements(env, password, pw, 0);
    if (!ok) {
        throw_state(env, "SPAKE2_generate_msg failed");
        return NULL;
    }
    jbyteArray out = (*env)->NewByteArray(env, (jsize)msg_len);
    if (out != NULL) (*env)->SetByteArrayRegion(env, out, 0, (jsize)msg_len, (const jbyte *)msg);
    return out;
}

JNIEXPORT jbyteArray JNICALL JNI_FN(nativeProcessMsg)(JNIEnv *env, jclass clazz, jlong handle,
                                                      jbyteArray their_msg) {
    (void)clazz;
    SPAKE2_CTX *ctx = (SPAKE2_CTX *)(intptr_t)handle;
    jsize len = (*env)->GetArrayLength(env, their_msg);
    if (ctx == NULL || len <= 0 || len > SPAKE2_MAX_MSG_SIZE) {
        throw_state(env, "SPAKE2 message has a bad size");
        return NULL;
    }
    uint8_t in[SPAKE2_MAX_MSG_SIZE];
    (*env)->GetByteArrayRegion(env, their_msg, 0, len, (jbyte *)in);
    uint8_t key[SPAKE2_MAX_KEY_SIZE];
    size_t key_len = 0;
    int ok = SPAKE2_process_msg(ctx, key, &key_len, sizeof(key), in, (size_t)len);
    if (!ok) {
        OPENSSL_cleanse(key, sizeof(key));
        throw_state(env, "SPAKE2_process_msg failed");
        return NULL;
    }
    jbyteArray out = (*env)->NewByteArray(env, (jsize)key_len);
    if (out != NULL) (*env)->SetByteArrayRegion(env, out, 0, (jsize)key_len, (const jbyte *)key);
    OPENSSL_cleanse(key, sizeof(key));
    return out;
}

JNIEXPORT void JNICALL JNI_FN(nativeFree)(JNIEnv *env, jclass clazz, jlong handle) {
    (void)env;
    (void)clazz;
    SPAKE2_CTX_free((SPAKE2_CTX *)(intptr_t)handle);
}
