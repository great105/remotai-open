import { describe, expect, it } from "vitest";
import { hermesViewport } from "./viewport";
const base = { layoutHeight: 844, baselineHeight: 844, visualHeight: 844, offsetTop: 0, scale: 1, focused: false, nativeHeight: 0 };
describe("Hermes single viewport owner", () => {
  it("pins to the visual viewport for a focused overlay keyboard", () => {
    expect(hermesViewport({...base, visualHeight: 444, focused:true})).toEqual({height:444, top:0, keyboard:true});
  });
  it("does not double-subtract a native keyboard when WebView has resized", () => {
    expect(hermesViewport({...base, layoutHeight:444, visualHeight:444, nativeHeight:400})).toEqual({height:444,top:0,keyboard:true});
  });
  it("subtracts only the native overlay remainder", () => {
    expect(hermesViewport({...base,nativeHeight:300})).toEqual({height:544,top:0,keyboard:true});
  });
  it("does not mistake pinch zoom or an unfocused landscape viewport for keyboard", () => {
    expect(hermesViewport({...base,focused:true,visualHeight:400,scale:2})).toEqual({height:844,top:0,keyboard:false});
    expect(hermesViewport({...base,layoutHeight:360,visualHeight:360})).toEqual({height:360,top:0,keyboard:false});
  });
  it("keeps the visual offset", () => {
    expect(hermesViewport({...base,focused:true,visualHeight:400,offsetTop:40})).toEqual({height:400,top:40,keyboard:true});
  });
});
