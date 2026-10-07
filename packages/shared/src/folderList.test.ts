import { describe, expect, it } from "vitest";
import { folderPathKey, selectFolders } from "./folderList";

const options = {query:"",sort:"name-asc" as const,locale:"ru-RU"};
describe("folder list choices", () => {
  it("orders numbered names naturally in both directions without changing the source", () => {
    const items = [{name:"Проект 10",path:"/10"},{name:"Проект 2",path:"/2"},{name:"Проект 1",path:"/1"}];
    const original = structuredClone(items);
    expect(selectFolders(items,options).map(i=>i.name)).toEqual(["Проект 1","Проект 2","Проект 10"]);
    expect(selectFolders(items,{...options,sort:"name-desc"}).map(i=>i.name)).toEqual(["Проект 10","Проект 2","Проект 1"]);
    expect(items).toEqual(original);
  });
  it("uses recent-folder visits and project/file modification times, with unknown dates last", () => {
    const items = [{name:"Unknown",path:"/a",modified:null},{name:"Old",path:"/b",modified:100},{name:"New",path:"/c",modified:200},{name:"Visited",path:"/d",time:300}];
    expect(selectFolders(items,{...options,sort:"date-desc"}).map(i=>i.name)).toEqual(["Visited","New","Old","Unknown"]);
  });
  it("matches Windows favorites across spelling differences while preserving Unix case", () => {
    expect(folderPathKey("C:\\Work\\PROJECT\\")).toBe(folderPathKey("c:/work/project"));
    expect(folderPathKey("\\\\PC\\Share\\Project")).toBe(folderPathKey("//pc/share/project/"));
    expect(folderPathKey("/work/PROJECT")).not.toBe(folderPathKey("/work/project"));
    expect(folderPathKey("/work/a\\b")).not.toBe(folderPathKey("/work/a/b"));
    const items=[{name:"A",path:"C:\\Work\\Project"},{name:"B",path:"/work/PROJECT"}];
    expect(selectFolders(items,{...options,favorites:[{path:"c:/work/project/"},{path:"/work/project"}]}).map(i=>i.name)).toEqual(["A"]);
  });
  it("can search only names instead of matching every child of the same parent", () => {
    const items=[{name:"Alpha",path:"/Work/Alpha"},{name:"beta",path:"/Work/beta"},{name:".hidden",path:"/Work/.hidden"}];
    expect(selectFolders(items,{...options,query:" Work ",searchPath:false})).toEqual([]);
    expect(selectFolders(items,{...options,query:"Work",searchPath:true,hideDotFolders:true})).toHaveLength(2);
    expect(selectFolders(items,{...options,query:" BETA ",searchPath:false}).map(i=>i.name)).toEqual(["beta"]);
  });
  it("searches the translated label of a standard location", () => {
    const items=[{name:"Desktop",path:"/home/test/Desktop"}];
    expect(selectFolders(items,{...options,query:"стол",label:()=>"Рабочий стол"})).toEqual(items);
  });
});
