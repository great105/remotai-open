# TGControl — R8/ProGuard rules for the minified release build.
#
# The web UI is a pre-minified Vite bundle inside the WebView, so R8 only
# touches the native Capacitor/Android layer. These keeps make sure plugin
# discovery (reflection + annotations) and JS bridges survive shrinking.

# Preserve annotations R8 uses for Capacitor plugin discovery and JS bridge.
-keepattributes *Annotation*
-keepattributes JavascriptInterface

# Keep readable stack traces so Sentry can deobfuscate native crashes.
-keepattributes SourceFile,LineNumberTable
-renamesourcefileattribute SourceFile

# Capacitor core + first-party plugins are resolved by name/reflection.
-keep class com.getcapacitor.** { *; }
-keep class com.capacitorjs.** { *; }
-keep @com.getcapacitor.annotation.CapacitorPlugin public class * { *; }
-keepclassmembers class * {
    @com.getcapacitor.annotation.PluginMethod <methods>;
}

# Cordova plugins bridged through Capacitor.
-keep class org.apache.cordova.** { *; }

# Anything exposed to JavaScript via @JavascriptInterface must keep its members.
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}

# androidx WebKit (used for modern WebView features) — keep public surface.
-keep class androidx.webkit.** { *; }
