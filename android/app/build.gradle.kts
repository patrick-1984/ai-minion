plugins {
    id("com.android.application") version "8.9.2"
    id("org.jetbrains.kotlin.android") version "2.1.21"
}

val minionVersion = providers.gradleProperty("minionVersion").orElse("0.0.0-rc.1").get()
val parts = Regex("""(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.([1-9][0-9]*))?""")
    .matchEntire(minionVersion)?.groupValues ?: error("Expected a Minion version X.Y.Z or X.Y.Z-rc.N")
val (major, minor, patch) = parts.subList(1, 4).map(String::toLong)
val rc = parts[4].ifEmpty { "999" }.toLong()
require(major <= 209 && minor <= 99 && patch <= 99 && (parts[4].isEmpty() || rc <= 998)) {
    "The Minion version does not fit the Android version code"
}
val nativePackage = providers.gradleProperty("minionNativePackage").map(::file)
// Device control (on-device adb, design docs/minion-android-adb/DESIGN.md): compiled in, off by default.
// Only a build with -PminionDeviceControl=true (the gate RC) declares its components, permission and native
// library; every other build, release included, has no reachable device-control entry point.
val deviceControl = providers.gradleProperty("minionDeviceControl").map { it == "true" }.getOrElse(false)

android {
    namespace = "cc.nasz.minion"
    compileSdk = 35
    buildToolsVersion = "35.0.0"

    defaultConfig {
        applicationId = "cc.nasz.minion"
        minSdk = 29
        targetSdk = 35
        versionCode = (major * 10_000_000 + minor * 100_000 + patch * 1_000 + rc).toInt()
        versionName = minionVersion
        ndk.abiFilters += listOf("arm64-v8a", "armeabi-v7a")
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    sourceSets.named("main") {
        if (nativePackage.isPresent) {
            jniLibs.srcDir(nativePackage.get().resolve("lib"))
            assets.srcDir(nativePackage.get().resolve("assets"))
        }
    }

    buildFeatures.buildConfig = true
    defaultConfig.buildConfigField("boolean", "DEVICE_CONTROL", deviceControl.toString())

    if (deviceControl) {
        ndkVersion = "28.2.13676358"
        // Overlay manifest: provider, pairing activity, SYSTEM_ALERT_WINDOW. Both build types get it.
        for (type in listOf("debug", "release")) {
            sourceSets.named(type) { manifest.srcFile("src/deviceControl/AndroidManifest.xml") }
        }
        // Licence notice of the BoringSSL code inside libminion_spake2.so (assets/licenses/boringssl-LICENSE.txt).
        sourceSets.named("main") { assets.srcDir("src/deviceControl/assets") }
        externalNativeBuild.cmake {
            path = file("src/main/cpp/CMakeLists.txt")
            version = "3.31.6"
        }
        // Optional offline checkout of the pinned BoringSSL tag (else CMake downloads it and checks the hash).
        providers.gradleProperty("minionBoringsslDir").orNull?.let { dir ->
            defaultConfig.externalNativeBuild.cmake.arguments += "-DMINION_BORINGSSL_DIR=$dir"
        }
    }

    testOptions.unitTests.all { test ->
        providers.gradleProperty("minionSpake2HostLib").orNull?.let { test.systemProperty("minion.spake2.lib", it) }
    }

    packaging.jniLibs {
        // Package Manager extracts the PIEs into APK-owned nativeLibraryDir.
        // These are already stripped and still contain Go build information.
        useLegacyPackaging = true
        keepDebugSymbols += "**/lib*.so"
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    testImplementation("junit:junit:4.13.2")
}

// Belt and braces: without the flag the SPAKE2 library never goes into any APK.
if (!deviceControl) {
    androidComponents.onVariants { variant -> variant.packaging.jniLibs.excludes.add("**/libminion_spake2.so") }
}
