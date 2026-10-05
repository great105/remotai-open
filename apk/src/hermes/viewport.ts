export interface HermesViewportInput {
  layoutHeight:number; baselineHeight:number; visualHeight:number; offsetTop:number; scale:number; focused:boolean; nativeHeight:number;
}
export function hermesViewport(input:HermesViewportInput) {
  // Pinch zoom is reading, not IME. Native resize and visual overlay share one owner.
  if (Math.abs(input.scale - 1) > .05) return {height:input.layoutHeight, top:0, keyboard:false};
  const visualKeyboard = input.focused && input.layoutHeight - input.visualHeight > 120;
  const keyboard = input.nativeHeight > 0 || visualKeyboard;
  const nativeBottom = input.nativeHeight > 0 ? input.baselineHeight - input.nativeHeight : input.layoutHeight;
  const top = visualKeyboard ? Math.max(0, input.offsetTop) : 0;
  const height = keyboard ? Math.max(1, Math.min(input.layoutHeight - top, visualKeyboard ? input.visualHeight : input.layoutHeight, nativeBottom - top)) : input.layoutHeight;
  return {height, top, keyboard};
}
