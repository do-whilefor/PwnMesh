(function(root,factory){const model=factory();if(typeof module==='object'&&module.exports)module.exports=model;else root.PwnDemoModel=model;})(typeof globalThis!=='undefined'?globalThis:this,function(){
  'use strict';
  const MiB=1048576;
  const PROJECT_TYPES=Object.freeze([
    Object.freeze({value:'ctf',label:'CTF',icon:'flag'}),
    Object.freeze({value:'audit',label:'代码审计',icon:'code-2'}),
    Object.freeze({value:'pentest',label:'渗透测试',icon:'shield-check'})
  ]);
  const asText=value=>typeof value==='string'?value:'';
  const fileMetadata=file=>file;
  function validateFiles(files=[],existing=[]){
    if(!Array.isArray(files)||!Array.isArray(existing))return '请选择有效的文件。';
    const all=[...existing,...files];
    if(all.length>32)return '每个项目最多选择 32 个文件（含已导入材料）。';
    if(all.some(file=>!file||!Number.isSafeInteger(file.size)||file.size<0))return '文件大小必须是有效的非负整数。';
    if(all.some(file=>file.size>256*MiB))return '单个文件不能超过 256 MiB。';
    if(all.reduce((total,file)=>total+file.size,0)>512*MiB)return '文件总大小不能超过 512 MiB（含已导入材料）。';
    if(all.some(file=>typeof file.name!=='string'||!file.name.trim()||/[\\/\u0000-\u001f\u007f]/.test(file.name)||new TextEncoder().encode(file.name).length>200))return '文件名须为不含路径或控制字符的普通名称，长度不超过 200 字节。';
    return '';
  }
  function validateProject(data={}){
    const name=asText(data.name).trim(),target=asText(data.target).trim(),goal=asText(data.goal).trim();
    if(!name)return '请填写项目名称。';
    if(name.length>200)return '项目名称不能超过 200 个字符。';
    if(!target)return '请填写目标或已知信息。';
    if(!goal)return '请填写项目目标。';
    if(target.length>32768||goal.length>32768)return '目标与已知信息分别不能超过 32768 个字符。';
    if(!PROJECT_TYPES.some(type=>type.value===data.type))return '请选择项目类型。';
    return validateFiles(data.files||[]);
  }
  function isProjectClosed(project){return !!project?.archived || ['done','completed','ended','finished','terminated','archived'].includes(project?.status);}
  function validateHint(data={},project){
    if(!project)return '项目不存在或已移除。';
    if(isProjectClosed(project))return '项目已结束，无法补充信息。';
    const text=asText(data.text).trim(),files=data.files||[];
    if(text.length>32768)return '补充信息不能超过 32768 个字符。';
    const error=validateFiles(files,project.files||[]);
    if(error)return error;
    if(!text&&!files.length)return '请填写补充信息或选择文件。';
    return '';
  }
  function createFormDrafts(){
    const emptyCreate=()=>({name:'',target:'',goal:'',type:'pentest',files:[]});
    const clone=draft=>({...draft,files:draft.files.map(fileMetadata)});
    let create=emptyCreate();
    const hints=new Map();
    return {
      getCreate:()=>clone(create),
      setCreate:value=>{create={name:asText(value.name),target:asText(value.target),goal:asText(value.goal),type:value.type,files:value.files.map(fileMetadata)};},
      clearCreate:()=>{create=emptyCreate();},
      getHint:id=>clone(hints.get(id)||{text:'',files:[]}),
      setHint:(id,value)=>{hints.set(id,{text:asText(value.text),files:value.files.map(fileMetadata)});},
      clearHint:id=>hints.delete(id)
    };
  }
  function fileSize(bytes){if(bytes>=MiB)return (bytes/MiB).toFixed(1)+' MiB';if(bytes>=1024)return (bytes/1024).toFixed(1)+' KiB';return bytes+' B';}
  return {validateFiles,validateProject,validateHint,isProjectClosed,createFormDrafts,PROJECT_TYPES,fileSize,MiB};
});
