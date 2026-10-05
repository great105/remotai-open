import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { HermesExecutionStatus } from "./HermesExecutionStatus";
describe("compact real child activity", () => {
 it("uses the existing calm lifecycle row rather than hiding action until Work opens", () => {
  const html = renderToStaticMarkup(createElement(HermesExecutionStatus,{busy:true,waiting:false,progress:"",answerStarted:false,hasAnswer:true,restoring:false,subagents:[{id:"child",goal:"Read tests",status:"running",toolCount:1,lastTool:"read_file"}],subagentError:"", observableActivity:{title:"Помощник работает",detail:"Последний запуск: Чтение файла"},onDetails:()=>{}}));
  expect(html).toContain("Помощник работает");
  expect(html).toContain("Последний запуск: Чтение файла");
  expect(html.match(/class="hermes-execution-status"/g)).toHaveLength(1);
 });
});
