import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { HermesSubagentProgress, subagentToolLabel, subagentStatusLabel } from "./HermesSubagentProgress";
import { emptySubagentProgress, observeSubagentEvent, observeSubagentRoster } from "./subagent-progress";
const child = () => observeSubagentEvent(observeSubagentRoster(emptySubagentProgress("scope","live"),[{id:"child",goal:"Проверить большой проект",status:"running",toolCount:3,startedAt:100}],2000),{seq:1,frame:{method:"event",params:{session_id:"live",type:"subagent.tool",payload:{subagent_id:"child",tool_name:"read_file",text:"PRIVATE",tool_preview:"PRIVATE"}}}},3000).children[0];
const render = (selectedId:string,nativeState = "unsupported" as ReturnType<typeof child>["nativeState"]) => renderToStaticMarkup(createElement(HermesSubagentProgress,{children:[{...child(),nativeState}],selectedId,onSelect:()=>{}}));
describe("observable child details",()=>{
 it("keeps an actual failed roster status visible without inventing a terminal event",()=>{
  expect(subagentStatusLabel({...child(),status:"failed",terminalConfirmed:false})).toContain("Ошибка");
 });
 it("uses generic text for unknown and prototype-like tool names",()=>{
  for(const name of ["constructor","toString","unknown-tool","terminal(secret)"])expect(subagentToolLabel(name)).toBe("Инструмент Hermes");
 });
 it("shows owned current task and a truthful last launch with separately labelled observation times",()=>{
  const html = render("child");
  for(const text of ["Текущая задача","Проверить большой проект","Последний запуск: Чтение файла","Начат","Событие получено","Статус проверен","Журнал действий","Результаты инструментов недоступны на этой версии компьютера"]) expect(html).toContain(text);
  expect(html).not.toContain("PRIVATE");
  expect(html).not.toContain("Сейчас читает");
 });
 it("keeps the detailed 200-entry journal collapsed by default",()=>{
  expect(render("")).not.toContain("Событие получено");
  expect(render("")).toContain('aria-expanded="false"');
 });
 it("renders logged results separately with ambiguous source timezone, never a paired call",()=>{
  const row = {...child(),nativeState:"supported" as const,nativeJournal:[{id:"result",kind:"tool_result" as const,toolName:"terminal",sourceTimeText:"12:34:56",status:"error" as const,durationSeconds:1.2}]};
  const html = renderToStaticMarkup(createElement(HermesSubagentProgress,{children:[row],selectedId:"child",onSelect:()=>{}}));
  expect(html).toContain("Результат: Команда · ошибка · 1.2 с");
  expect(html).toContain("12:34:56");
  expect(html).toContain("Часовой пояс компьютера неизвестен");
 });
});
