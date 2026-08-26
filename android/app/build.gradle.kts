import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// ---- Release signing (optional) ----
// Reads android/keystore.properties (storeFile/storePassword/keyAlias/
// keyPassword — NEVER committed; see android/.gitignore), falling back to
// XXDRIVE_* env vars. With all four present, assembleRelease signs; without
// them it logs clearly and produces an unsigned APK. Relative storeFile
// paths resolve against the Gradle root (android/) directory.
val keystoreProps = Properties()
rootProject.file("keystore.properties").takeIf { it.exists() }?.inputStream()?.use {
    keystoreProps.load(it)
}

fun signingProp(name: String, envVar: String): String? =
    keystoreProps.getProperty(name)?.trim()?.takeIf { it.isNotEmpty() }
        ?: System.getenv(envVar)?.trim()?.takeIf { it.isNotEmpty() }

val releaseStoreFile = signingProp("storeFile", "XXDRIVE_STORE_FILE")
val releaseStorePassword = signingProp("storePassword", "XXDRIVE_STORE_PASSWORD")
val releaseKeyAlias = signingProp("keyAlias", "XXDRIVE_KEY_ALIAS")
val releaseKeyPassword = signingProp("keyPassword", "XXDRIVE_KEY_PASSWORD")
val releaseSigningReady = releaseStoreFile != null &&
    releaseStorePassword != null && releaseKeyAlias != null && releaseKeyPassword != null

android {
    namespace = "com.piercingxx.xxdrive"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.piercingxx.xxdrive"
        minSdk = 26
        targetSdk = 35
        versionCode = 2
        versionName = "1.1.0"
    }

    signingConfigs {
        if (releaseSigningReady) {
            create("release") {
                storeFile = rootProject.file(releaseStoreFile!!)
                storePassword = releaseStorePassword
                keyAlias = releaseKeyAlias
                keyPassword = releaseKeyPassword
            }
        }
    }

    buildTypes {
        release {
            // Minify stays OFF deliberately — explicit v1 non-goal (todo.md):
            // no R8/shrinker pass while the WebView/PWA surface is churning.
            isMinifyEnabled = false
            if (releaseSigningReady) {
                signingConfig = signingConfigs.getByName("release")
                logger.lifecycle("xx-drive: assembleRelease will be SIGNED (alias '$releaseKeyAlias').")
            } else {
                val touched = listOf(releaseStoreFile, releaseStorePassword, releaseKeyAlias, releaseKeyPassword)
                    .any { it != null }
                val detail = if (touched) {
                    val missing = mutableListOf<String>().apply {
                        if (releaseStoreFile == null) add("storeFile/XXDRIVE_STORE_FILE")
                        if (releaseStorePassword == null) add("storePassword/XXDRIVE_STORE_PASSWORD")
                        if (releaseKeyAlias == null) add("keyAlias/XXDRIVE_KEY_ALIAS")
                        if (releaseKeyPassword == null) add("keyPassword/XXDRIVE_KEY_PASSWORD")
                    }
                    "PARTIAL signing material ignored — missing: $missing"
                } else {
                    "no keystore.properties or XXDRIVE_* env vars found"
                }
                logger.warn("xx-drive: $detail — assembleRelease will produce UNSIGNED APK(s).")
            }
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    testOptions {
        // Android SDK stubs return defaults instead of throwing in JVM unit
        // tests, so pure-logic tests can touch classes that reference android.*.
        unitTests.isReturnDefaultValues = true
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.webkit:webkit:1.11.0")
    implementation("androidx.work:work-runtime-ktx:2.9.1")
    implementation("com.google.android.material:material:1.12.0")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    // Encrypted credential storage for Session (bearer token at rest).
    implementation("androidx.security:security-crypto:1.1.0-alpha06")

    testImplementation("junit:junit:4.13.2")
}
