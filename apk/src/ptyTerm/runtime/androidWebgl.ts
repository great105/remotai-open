/** Android WebView can expose WebGL2 through SwiftShader while corrupting glyphs. */
export function androidWebglNeedsDom(
  native: boolean,
  userAgent: string,
  createCanvas: () => HTMLCanvasElement = () => document.createElement("canvas"),
): boolean {
  if (!native || !/\bAndroid\b/i.test(userAgent)) return false;
  try {
    const gl = createCanvas().getContext("webgl2");
    if (!gl) return false;
    const rendererInfo = gl.getExtension("WEBGL_debug_renderer_info");
    return !!rendererInfo && /swiftshader/i.test(String(gl.getParameter(rendererInfo.UNMASKED_RENDERER_WEBGL)));
  } catch {
    return false;
  }
}
