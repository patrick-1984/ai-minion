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
