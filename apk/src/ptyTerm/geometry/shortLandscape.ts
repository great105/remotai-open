/** Physical screen orientation stays stable when the Android keyboard resizes the viewport. */
export function isShortLandscape(screenWidth: number, screenHeight: number, viewportHeight: number): boolean {
  return screenWidth > screenHeight && viewportHeight <= 520;
}
