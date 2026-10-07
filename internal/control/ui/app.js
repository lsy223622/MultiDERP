'use strict';
let actor, csrf, serverRole;
let pageGeneration=0, currentPage='', refreshTimer;
let resourceScope='mine', userNames=new Map();
const $ = id => document.getElementById(id);
function notice(message, error = false) { const n = $('notice'); n.textContent = message; n.classList.toggle('error',error); n.hidden = false; clearTimeout(notice.timer); notice.timer = setTimeout(() => n.hidden = true,7000); }
async function api(path,method='GET',body) {
  const generation=pageGeneration;
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && csrf) headers['X-CSRF-Token'] = csrf;
  const r = await fetch('/api/v1'+path,{method,headers,credentials:'same-origin',body:body===undefined?undefined:JSON.stringify(body)});
  const result = await r.json().catch(() => ({}));
  if(generation!==pageGeneration)throw new DOMException('Page changed','AbortError');
  if (!r.ok) { const e = new Error(({invalid_hostname:'请填写纯域名，例如 derp.example.com，不包含 https://、端口或路径。',certificate_required:'请先上传证书链和私钥，再应用设置。',certificate_invalid:'证书链或私钥无法配对，请重新上传有效的 PEM 文件。',certificate_hostname_mismatch:'证书域名与已保存的公共 DERP 域名不匹配。'})[result.code]||({400:'输入不符合要求，请检查字段。',401:'请重新登录。',403:'你没有执行此操作的权限。',409:'状态已改变，请刷新后重试。',429:'请求过于频繁，请稍后再试。',503:'身份源暂不可用。请核对 Tailnet ID、OAuth Client Secret、只读设备权限和主控的 API 网络，再重试。'})[r.status] || '请求失败，请检查资源状态后重试。'); e.status=r.status;e.code=result.code;e.field=result.field; throw e; }
  return result;
}
function el(tag,text,className) { const e=document.createElement(tag); if(text!==undefined)e.textContent=text; if(className)e.className=className; return e; }
function button(label,run,className='quiet') { const b=el('button',label,className); b.type='button'; b.addEventListener('click',async()=>{b.disabled=true;try{await run();}catch(e){if(e.name!=='AbortError')notice(e.message,true);}finally{b.disabled=false;}});return b; }
function page(title,hint) { $('content').replaceChildren(el('h1',title),el('p',hint,'muted')); }
function card(title,parent=$('content')) { const c=el('section',undefined,'card');c.append(el('h2',title));parent.append(c);return c; }
function detailCard(title,id){
 const trigger=document.activeElement,c=card(title);c.id=id;
 c.prepend(button('关闭详情',()=>{c.remove();if(trigger?.isConnected)trigger.focus();}));
 const heading=c.querySelector('h2');heading.tabIndex=-1;heading.focus();c.scrollIntoView({behavior:'smooth',block:'start'});return c;
}
async function scopeSelector(){
 if(actor.role!=='admin')return;
 const users=await api('/users');userNames=new Map(users.map(u=>[u.id,u.username]));
 const row=el('div',undefined,'row scope-selector'),input=select(row,'scope','资源范围',[['mine','我的'],['all','全部']]);input.value=resourceScope;
 input.addEventListener('change',()=>{resourceScope=input.value;render({preserveScroll:true});});$('content').append(row);
}
function managedLabel(owner){return owner!==actor.id?'代 '+(userNames.get(owner)||owner.slice(0,8))+' 管理':'';}
function nextActions({actor,serverRole,local,nodes,tailnets,grants}){
 if(serverRole==='member')return [{title:local.pending_apply?'应用本机设置':!local.joined?'加入已有集群':'查看主控连接与许可',href:local.pending_apply?'local':'member',path:'本机节点'}];
 const actions=[],ownTailnets=tailnets.filter(t=>t.owner_id===actor.id),ownNodes=nodes.filter(n=>n.owner_id===actor.id);
 if(actor.role==='admin'||actor.role==='provider'){
  const incoming=grants.filter(g=>g.node_owner_id===actor.id&&g.state==='requested');
  if(incoming.length)actions.push({title:'批准 '+incoming.length+' 条节点使用申请',href:'requests',path:'提供节点'});
  if(!ownNodes.length)actions.push({title:'添加独立 DERP 节点',href:'nodes',path:'提供节点'});
  else if(ownNodes.some(n=>n.state==='pending'))actions.push({title:'查看注册信息，让子节点加入',href:'nodes',path:'提供节点'});
  if(actor.role==='admin'&&local&&!local.joined)actions.push({title:local.pending_apply?'应用内置节点设置（可选）':'注册主控内置节点（可选）',href:local.pending_apply?'local':'member',path:'提供节点'});
 }
 const own=grants.filter(g=>g.tailnet_owner_id===actor.id);
 if(!ownTailnets.length)actions.push({title:'绑定自己的 Tailnet',href:'tailnets',path:'使用 DERP'});
 else if(own.some(g=>g.state==='owner_approved'))actions.push({title:'确认提供者已批准的使用授权',href:'grants',path:'使用 DERP'});
 else if(own.some(g=>g.state==='requested'))actions.push({title:'查看申请，等待提供者批准',href:'grants',path:'使用 DERP'});
 else if(!own.some(g=>g.state==='active'))actions.push({title:'为 Tailnet 选择节点',href:'directory',path:'使用 DERP'});
 else actions.push({title:'复制 DERP map，手动合并 Tailnet 策略',href:'tailnets',path:'使用 DERP'});
 return actions;
}
async function copyText(text){try{await navigator.clipboard.writeText(text);notice('已复制。');}catch{notice('复制失败，请选中文本手动复制。',true);}}
function limitedRefresh(parent,read){
 const generation=pageGeneration,started=Date.now(),message=el('p','每 5 秒读取相关状态，最多 2 分钟。','muted');parent.append(message);
 const refresh=async()=>{
  if(generation!==pageGeneration||document.hidden||!parent.isConnected)return;
  try{const pending=await read();if(generation!==pageGeneration||!parent.isConnected)return;
   if(!pending){message.textContent='状态已更新 · '+new Date().toLocaleString();return;}
   if(Date.now()-started>=120000){message.textContent='自动读取已结束，可手动刷新状态。';parent.append(button('刷新状态',refresh));return;}
   refreshTimer=setTimeout(refresh,5000);
  }catch(e){if(e.name==='AbortError')return;message.textContent='状态读取失败，可手动刷新。';parent.append(button('刷新状态',refresh));}
 };refreshTimer=setTimeout(refresh,5000);
}
function field(form,name,label,type='text',required=true,value='') { const l=el('label',label);const input=el('input');input.name=name;input.type=type;input.required=required;input.value=value;if(name==='domain'||name==='hostname'){input.placeholder='derp.example.com';l.append(el('small','填写纯域名，不包含协议、端口或路径。','muted'));}l.append(input);form.append(l);return input; }
function select(form,name,label,items) { const l=el('label',label);const input=el('select');input.name=name;for(const [value,text] of items){const o=el('option',text);o.value=value;input.append(o);}l.append(input);form.append(l);return input; }
function form(parent,submit,run) { const f=el('form');parent.append(f);const b=el('button',submit);b.type='submit';f.addEventListener('submit',async e=>{e.preventDefault();const generation=pageGeneration;b.disabled=true;f.querySelectorAll('.field-error').forEach(n=>n.remove());f.querySelectorAll('[aria-invalid]').forEach(n=>n.removeAttribute('aria-invalid'));try{const after=await run(new FormData(f),f);if(generation!==pageGeneration)return;notice(f.successMessage||submit+'：已保存。');if(await render({preserveScroll:true})&&typeof after==='function')await after();}catch(err){if(err.name==='AbortError')return;const input=err.field&&f.elements.namedItem(err.field);if(input){input.setAttribute('aria-invalid','true');input.after(el('small',err.message,'field-error'));input.focus();}notice(err.message,true);}finally{b.disabled=false;}});f.submitButton=b;return f; }
function done(f){f.append(f.submitButton);return f;}
function table(parent,head,rows) { const wrap=el('div',undefined,'table-wrap'),t=el('table',undefined,'business-table'),tr=el('tr');for(const text of head)tr.append(el('th',text));const h=el('thead');h.append(tr);t.append(h);const body=el('tbody');for(const row of rows){const r=el('tr');for(const [i,value] of row.entries()){const c=el('td');c.dataset.label=head[i];c.append(value instanceof Node?value:el('span',value));r.append(c);}body.append(r);}t.append(body);wrap.append(t);parent.append(wrap);if(!rows.length)parent.append(el('p','暂无记录。','muted')); }
let menuSequence=0;
function configureMenu(trigger,menu){
 trigger.setAttribute('aria-haspopup','menu');trigger.setAttribute('aria-expanded','false');menu.setAttribute('role','menu');
 menu.querySelectorAll('button,a,summary').forEach(item=>{if(!item.hasAttribute('role'))item.setAttribute('role','menuitem');});
 menu.addEventListener('toggle',e=>{
  const open=e.newState==='open';trigger.setAttribute('aria-expanded',String(open));if(!open){menu.querySelectorAll('details').forEach(d=>d.open=false);return;}
  const rect=trigger.getBoundingClientRect(),width=menu.offsetWidth,height=menu.offsetHeight;
  const above=rect.bottom+height+4>innerHeight;
  menu.style.left=Math.max(8,Math.min(rect.right-width,innerWidth-width-8))+'px';
  menu.style.top=above?'auto':Math.max(8,rect.bottom+4)+'px';
  menu.style.bottom=above?Math.max(8,innerHeight-rect.top+4)+'px':'auto';
  menu.style.transformOrigin=above?'bottom right':'top right';
  menu.querySelector('button:not([hidden]),a:not([hidden])')?.focus();
 });
 menu.addEventListener('click',e=>{if(e.target.closest('button,a'))menu.hidePopover();});
 menu.addEventListener('keydown',e=>{
  const items=[...menu.querySelectorAll('button:not(:disabled):not([hidden]),a:not([hidden]),summary')].filter(item=>item.tagName==='SUMMARY'||!item.closest('details')||item.closest('details').open);let index=items.indexOf(document.activeElement);
  if(e.key==='ArrowDown')index=(index+1)%items.length;
  else if(e.key==='ArrowUp')index=(index-1+items.length)%items.length;
  else if(e.key==='Home')index=0;else if(e.key==='End')index=items.length-1;else return;
  e.preventDefault();items[index]?.focus();
 });
}
function more(...items){const wrap=el('span'),trigger=el('button',undefined,'more-menu'),menu=el('div',undefined,'more-options');trigger.type='button';trigger.setAttribute('aria-label','更多操作');trigger.innerHTML='<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h.01M12 12h.01M19 12h.01"/></svg>';menu.id='action-menu-'+(++menuSequence);menu.setAttribute('popover','auto');trigger.setAttribute('popovertarget',menu.id);menu.append(...items);configureMenu(trigger,menu);wrap.append(trigger,menu);return wrap;}
function actions(...items){const r=el('div',undefined,'row');r.append(...items);return r;}
function time(value){if(!value||value==='0001-01-01T00:00:00Z')return '尚无观测';return new Date(typeof value==='number'?value*1000:value).toLocaleString();}
function bytes(value){const text=el('span');text.title=String(value)+' 字节';text.textContent=value>=1048576?(value/1048576).toFixed(2)+' MiB':value>=1024?(value/1024).toFixed(2)+' KiB':value+' B';return text;}
function shortIdentity(id,name){const details=el('details',undefined,'identity');details.append(el('summary',name||id.slice(0,8)+'…'),el('code',id),button('复制完整 ID',()=>copyText(id)));return details;}
async function visibleTailnetNames(){const [tailnets,grants]=await Promise.all([api('/tailnets'),api('/grants')]);const names=new Map(grants.map(g=>[g.tailnet_id,g.tailnet_name+' · '+g.applicant]));for(const t of tailnets)if(!names.has(t.id))names.set(t.id,t.display_name);return {tailnets,names};}
function reportedBudget(parent,state){
 const r=state.report;
 if(r&&r.revision!==state.desired_revision)parent.append(el('p','规则更新中，以下为最近节点报告。','badge warn'));
 table(parent,['节点报告项目','值','来源'],[
  ['本地总上限',r?.local_max_budget_bps===undefined?'尚无节点报告':r.local_max_budget_bps===0?'不另设上限':r.local_max_budget_bps/1000000+' Mbps',r?'采样于 '+time(r.observed_at):'尚无节点报告'],
  ['实际执行预算',r?.effective_budget_bps===undefined?'尚无节点报告':r.effective_budget_bps/1000000+' Mbps',r?'版本 '+r.revision:'尚无节点报告']
 ]);parent.append(el('p','以上是预算和运行配置观测，不代表吞吐、保底带宽或连通性承诺。','muted'));
}
function status(value){const names={pending:'待注册',domain_pending:'等待新域名验证',registered:'已注册',ready:'已报告可用',offline:'离线',identity_conflict:'身份冲突',requested:'待提供者批准',owner_approved:'待申请方确认',active:'活跃',revoked:'已撤销',left:'已退出',rejected:'已拒绝',cancelled:'已取消',expired:'已到期',valid:'正常',invalid:'凭据异常',missing:'未配置',paused:'已暂停',unavailable:'身份源暂不可用'};return el('span',names[value]||value||'尚无状态','badge '+(['ready','active','valid'].includes(value)?'good':['identity_conflict','invalid','revoked'].includes(value)?'bad':'warn'));}
function secretFields(f){const input=field(f,'client_secret','OAuth Client Secret · ','password');input.autocomplete='new-password';const link=el('a','OAuth 文档');link.href='https://tailscale.com/docs/features/oauth-clients';link.target='_blank';link.rel='noopener noreferrer';input.before(link,el('span',' · 只需 devices:core:read','muted'));return input;}
async function tailnets(){
  page('我的 Tailnet','使用稳定的 Tailnet ID 绑定身份源。OAuth 只请求设备只读权限，原始客户端权限由所有者配置。');
  await scopeSelector();
  const [allItems,retention,nodes,grants]=await Promise.all([api('/tailnets'),api('/settings/retention'),api('/relays'),api('/grants')]);
  const items=allItems.filter(t=>actor.role!=='admin'||resourceScope==='all'||t.owner_id===actor.id);
  const summaries=await Promise.all(nodes.map(n=>api('/nodes/'+n.id+'/summary'))),relays=new Map(summaries.map(s=>[s.node.id,s]));
  const available=grants.filter(g=>g.state==='active');
  for(const t of items){
    const entries=(t.enabled?available.filter(g=>g.tailnet_id===t.id):[]).map(g=>({grant:g,relay:relays.get(g.node_id)})).filter(x=>x.relay);
    const c=el('section',undefined,'card resource-card');
    const header=el('div',undefined,'resource-header'),disclosure=el('details',undefined,'resource-disclosure'),summary=el('summary');
    summary.append(el('strong',t.display_name),el('code',t.api_id),el('span',entries.length+' 个可用 DERP','badge'));
    if(actor.role==='admin'&&t.owner_id!==actor.id)summary.append(el('span',managedLabel(t.owner_id),'badge'));
    const details=el('div',undefined,'resource-details'),facts=el('div',undefined,'resource-facts');
    facts.append(actions(status(t.enabled?'active':'paused'),status(t.credential_status)),el('span',t.last_identity_success?'完整身份成功 '+time(t.last_identity_success)+' · 缓存至 '+time(t.last_identity_success+retention.identity_seconds):'尚无完整身份记录','muted'));
    details.append(facts);
    table(details,['DERP 节点 / 提供者','域名 / 公开端口','节点状态 / 授权截止'],entries.map(({grant,relay})=>[
      actions(el('span',relay.node.display_name),el('span','提供者 '+relay.provider,'muted')),
      el('code',relay.node.domain+' · DERP TCP '+relay.node.derp_port+' / STUN UDP '+relay.node.stun_port),
      actions(status(relay.node.state),el('span',grant.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(grant.explicit_until)))
    ]));
    disclosure.append(summary,details);
    const mapPanel=el('div',undefined,'resource-map');mapPanel.hidden=true;
    const mapOutput=el('textarea');mapOutput.readOnly=true;mapOutput.setAttribute('aria-label',t.display_name+' DERP map JSON');mapOutput.spellcheck=false;
    mapPanel.append(el('h3','DERP map'),mapOutput);
    const mapMeta=el('p',undefined,'muted'),mapControls=actions(button('复制 JSON',()=>copyText(mapOutput.value)),button('下载 JSON',()=>{
      const url=URL.createObjectURL(new Blob([mapOutput.value],{type:'application/json'})),link=el('a');link.href=url;link.download='uniderp-'+t.id+'.json';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
    }));
    const instructions=el('ol');for(const text of ['打开 Tailscale 管理面板的 Access controls 策略。','把此 JSON 合并到策略顶层 derpMap 字段；保留现有 ACL、grants 和其他配置。','已有自定义 Region 时核对编号冲突；保留已有 Regions，不直接覆盖整个策略文件。不自动启用 OmitDefaultRegions。','保存策略后，用普通客户端检查实际 DERP 连接。复制或下载只完成导出。'])instructions.append(el('li',text));
    const docs=el('a','Tailscale 自定义 DERP 文档');docs.href='https://tailscale.com/docs/reference/derp-servers/custom-derp-servers';docs.target='_blank';docs.rel='noopener noreferrer';
    const empty=el('div');mapPanel.append(mapMeta,mapControls,instructions,docs,empty);
    mapPanel.id='tailnet-map-'+t.id;
    const controls=el('div',undefined,'resource-actions');
    const mapToggle=button('显示 DERP map',async()=>{
      if(!mapPanel.hidden){mapPanel.hidden=true;mapToggle.textContent='显示 DERP map';mapToggle.setAttribute('aria-expanded','false');return;}
      const result=await api('/tailnets/'+t.id+'/derpmap');mapOutput.value=JSON.stringify(result,null,2);const count=Object.values(result.Regions||{}).reduce((sum,r)=>sum+(r.Nodes?.length||0),0);mapMeta.textContent=count+' 个节点 · 本次读取 '+new Date().toLocaleString();empty.replaceChildren();
      if(!count){const related=grants.filter(g=>g.tailnet_id===t.id),waiting=related.some(g=>g.state==='requested'),confirming=related.some(g=>g.state==='owner_approved');empty.append(el('p',!t.enabled?'Tailnet 已暂停，当前没有可用地图。':confirming?'提供者已批准，请确认使用。':waiting?'正在等待提供者批准。':'当前没有具备有效设备许可的活跃授权。','muted'));const link=el('a',confirming?'确认使用':waiting?'查看申请':'选择节点');link.href=confirming||waiting?'#grants':'#directory';empty.append(link);}
      mapPanel.hidden=false;mapToggle.textContent='收起 DERP map';mapToggle.setAttribute('aria-expanded','true');
    });
    mapToggle.setAttribute('aria-controls',mapPanel.id);mapToggle.setAttribute('aria-expanded','false');controls.append(mapToggle);
    controls.append(more(button('替换凭据',()=>credential(t)),button(t.enabled?'暂停':'启用',async()=>{await api('/tailnets/'+t.id+'/enabled','POST',{enabled:!t.enabled});await render();}),button('删除凭据',async()=>{if(!confirm('删除凭据会撤销设备许可，确认继续？'))return;await api('/tailnets/'+t.id+'/credential','DELETE');await render();},'danger'),button('删除 Tailnet',async()=>{if(!confirm('删除 Tailnet 会终止所有相关授权。已离线节点的旧许可受原缓存期限约束。确认继续？'))return;await api('/tailnets/'+t.id,'DELETE');await render();},'danger'),...(actor.role==='admin'?[button('转移',()=>transfer(t))]:[])));
    header.append(disclosure,controls);c.append(header,mapPanel);$('content').append(c);
  }
  if(!items.length)$('content').append(el('p','尚未绑定 Tailnet。','muted'));
  const f=form(card('绑定 Tailnet'),'验证并绑定',async(data,f)=>{const body={display_name:data.get('name'),api_id:data.get('api_id'),credential:{client_secret:data.get('client_secret')}};f.elements.client_secret.value='';await api('/tailnets','POST',body);});
  field(f,'name','显示名称');field(f,'api_id','Tailnet ID（General Settings 中的 T… 标识）').autocomplete='off';secretFields(f);done(f);
}
async function transfer(t){
  const users=await api('/users'),c=card('转移 '+t.display_name);
  const f=form(c,'转移管理权',data=>api('/tailnets/'+t.id+'/transfer','POST',{owner_id:data.get('owner')}));
  select(f,'owner','新所有者',users.filter(u=>u.enabled).map(u=>[u.id,u.username]));done(f);
}
async function credential(t){const c=card('替换 '+t.display_name+' 的凭据');const f=form(c,'验证并替换',async(data,f)=>{const body={client_secret:data.get('client_secret')};f.elements.client_secret.value='';await api('/tailnets/'+t.id+'/credential','POST',body);});secretFields(f);done(f);c.scrollIntoView({behavior:'smooth'});}
async function nodes(){
  page(actor.role==='admin'?'节点管理':'我的节点','管理 DERP 节点的注册、公开端口、使用权限和带宽规则。暂停会下发空许可；域名或公开端口变更后，使用者需要更新 DERP map。');
  await scopeSelector();
  const items=(await api('/nodes')).filter(n=>actor.role!=='admin'||resourceScope==='all'||n.owner_id===actor.id),local=actor.role==='admin'?await api('/local/status'):null;
  if(local&&!local.joined)$('content').append(button('注册主控内置节点',async()=>{await api('/local/register','POST',{display_name:local.saved.hostname||'主控内置节点'});await render();},'primary'));
  const list=card('DERP 节点');
  const nodeActions=n=>actions(button('状态',()=>nodeState(n)),...(n.state!=='pending'?[button('规则',()=>qos(n,true))]:[]),...(local?.node_id===n.id?[button('本机管理',()=>{location.hash='member';})]:[]),more(button('公开端口',()=>nodePorts(n)),button(n.state==='pending'?'注册码':'重新签发注册码',()=>enrollment(n)),button(n.enabled?'暂停':'启用',async()=>{await api('/nodes/'+n.id+'/enabled','POST',{enabled:!n.enabled});await render();}),...(n.state!=='pending'?[button('域名',()=>changeDomain(n))]:[]),button('删除节点',()=>deleteNode(n),'danger')));
  table(list,['名称','域名 / 公开端口','状态','最近心跳','操作'],items.map(n=>[
    actions(el('span',n.display_name),...(actor.role==='admin'&&n.owner_id!==actor.id?[el('span',managedLabel(n.owner_id),'badge')]:[]),...(local?.node_id===n.id?[el('span','主控内置节点','badge')]:[])),el('code',n.domain+' · DERP TCP '+n.derp_port+' / STUN UDP '+n.stun_port),actions(status(n.state),...(!n.enabled?[status('paused')]:[])),time(n.last_heartbeat),
    nodeActions(n)
  ]));
  if(items.some(n=>n.state==='pending'||n.state==='registered'))limitedRefresh(list,async()=>{const fresh=await api('/nodes');let pending=false;for(const [i,n]of items.entries()){const updated=fresh.find(x=>x.id===n.id);if(!updated)continue;const row=list.querySelectorAll('tbody tr')[i];const stateChanged=n.state!==updated.state||n.enabled!==updated.enabled;Object.assign(n,updated);row.children[2].replaceChildren(status(n.state),...(!n.enabled?[status('paused')]:[]));row.children[3].textContent=time(n.last_heartbeat);if(stateChanged)row.children[4].replaceChildren(nodeActions(n));if(updated.state==='pending'||updated.state==='registered')pending=true;}return pending;});
  const f=form(card('添加 DERP 节点'),'创建节点并签发注册码',async data=>{const result=await api('/nodes','POST',{display_name:data.get('name'),domain:data.get('domain').trim(),derp_port:Number(data.get('derp_port')),stun_port:Number(data.get('stun_port'))});return()=>showEnrollment(result.enrollment,result.node);});field(f,'name','显示名称');field(f,'domain','公共 DERP 域名');publicPortFields(f,{derp_port:443,stun_port:3478});done(f);
}
function publicPortFields(f,n) {
  for(const [name,label] of [['derp_port','公开 DERP TCP 端口'],['stun_port','公开 STUN UDP 端口']]){const input=field(f,name,label,'number',true,n[name]);input.min='1';input.max='65535';input.step='1';}
}
async function nodePorts(n) {
  const c=detailCard(n.display_name+' · 公开端口','ports-'+n.id);
  c.append(el('p','填写客户端访问的宿主机映射或代理端口。先配置对应入口，再保存并更新使用者的 DERP map；节点域名注册证明仍使用 HTTPS 443。','muted'));
  const f=form(c,'保存公开端口',data=>api('/nodes/'+n.id+'/ports','POST',{derp_port:Number(data.get('derp_port')),stun_port:Number(data.get('stun_port'))}));
  publicPortFields(f,n);done(f);c.scrollIntoView({behavior:'smooth'});
}
async function changeDomain(n){
  const c=detailCard(n.display_name+' · 更换域名','domain-'+n.id);
  c.append(el('p','先在节点宿主机配置新域名的 DNS、TLS 和转发入口。提交时会暂停许可，节点随后以原私钥证明新域名；完成验证后仍需手动启用节点。使用者需要更新 DERP map。','muted'));
  const f=form(c,'暂停并验证新域名',async data=>{
    await api('/nodes/'+n.id+'/enabled','POST',{enabled:false});
    const state=await api('/nodes/'+n.id+'/status');
    if(state.control_stream_open && state.applied_revision!==state.desired_revision)throw new Error('已暂停，正在等待空许可应用 ACK。请刷新状态后再提交域名。');
    await api('/nodes/'+n.id+'/domain','POST',{domain:data.get('domain')});
  });field(f,'domain','新公共 DERP 域名','text',true,n.domain);done(f);
}
async function deleteNode(n){
  if(!confirm('删除节点会撤销集群身份和注册码，并移出 DERP map。离线许可到原缓存期限结束。确认继续？'))return;
  if(n.state!=='pending'){
    await api('/nodes/'+n.id+'/enabled','POST',{enabled:false});
    const state=await api('/nodes/'+n.id+'/status');
    if(state.control_stream_open && state.applied_revision!==state.desired_revision){await render();notice('节点已暂停，正在等待空许可应用 ACK。请刷新状态后再删除。',true);return;}
  }
  await api('/nodes/'+n.id,'DELETE');await render();
}
function showEnrollment(e,node){
 const c=detailCard((node?.display_name||'节点')+' · 注册信息','enrollment-'+(node?.id||'new')),address=location.origin;
 c.append(el('p','主控 HTTPS 地址'),el('pre',address),el('p','一次性注册码 · 有效至 '+time(e.expires_at)),el('pre',e.code));
 const steps=el('ol');for(const text of ['在子节点登录本机管理员，保存并应用本机设置。','打开“本机节点” → “加入已有集群”。','粘贴主控 HTTPS 地址和注册码，加入后返回这里查看结果。'])steps.append(el('li',text));c.append(steps);
 c.append(actions(button('复制地址',()=>copyText(address)),button('复制注册码',()=>copyText(e.code)),button('复制注册信息',()=>copyText('主控 HTTPS 地址：'+address+'\n一次性注册码：'+e.code+'\n有效至：'+time(e.expires_at)))));
 const expiry=el('p',undefined,'muted');c.append(expiry);
 if(node)limitedRefresh(c,async()=>{const updated=await api('/nodes/'+node.id);Object.assign(node,updated);expiry.textContent=Date.now()>=new Date(e.expires_at).getTime()?'注册码已到期，请重新生成。':'节点状态：'+(updated.state==='pending'?'等待子节点加入':'已注册，可查看观测状态');if(Date.now()>=new Date(e.expires_at).getTime()){c.append(button('重新生成',()=>enrollment(node)));return false;}return updated.state==='pending';});
}
async function enrollment(n){n=await api('/nodes/'+n.id);if(n.state!=='pending'&&!confirm('重新签发 '+n.display_name+' 会释放当前节点实例、关闭控制连接并撤销已有共享授权。重新加入后需要重新授权。确认继续？'))return;const e=await api('/nodes/'+n.id+'/enrollment','POST',{});if(await render({preserveScroll:true}))showEnrollment(e,n);}
function probeTable(parent,probes,node) {
  table(parent,['主控独立探测','结果','检查时间'],['derp','stun'].map(name=>[name==='derp'?'HTTPS DERP '+node.domain+':'+node.derp_port:'UDP STUN '+node.domain+':'+node.stun_port,probes?.[name]?.state==='ok'?'成功':probes?.[name]?.state==='failed'?'失败':'尚未检查',time(probes?.[name]?.observed_at)]));
  parent.append(el('p','探测仅表示主控当次能访问该端点。客户端是否能从所在网络连接，需要实际验证。','muted'));
}
function ruleSummary(parent,q) {
  const total=q.owner_weight+q.shared_weight;
  table(parent,['预算 / 组','争用时份额','硬上限'],[
    ['RX / TX 各 '+(q.budget_bps/1000000)+' Mbps','按实际字节计量','各方向受总预算限制'],
    ['自用组',(q.owner_weight/total*100).toFixed(1)+'%（权重 '+q.owner_weight+'）','总预算'],
    ['共享组',(q.shared_weight/total*100).toFixed(1)+'%（权重 '+q.shared_weight+'）',q.shared_max_bps?(q.shared_max_bps/1000000)+' Mbps':'未另设上限']
  ]);
  parent.append(el('p','份额用于双方均有需求时的分配；需求不足的一组会让出闲置容量，硬上限持续有效。组内 Tailnet 按权重分配，增加共享连接不会增加份额。','muted'));
}
async function nodeState(n) {
  const [s,{names}]=await Promise.all([api('/nodes/'+n.id+'/status'),visibleTailnetNames()]),c=detailCard(n.display_name+' · 观测状态','state-'+n.id);
  reportedBudget(c,s);
  c.append(actions(status(s.node.state),...(!s.node.enabled?[status('paused')]:[]),button('刷新观测',async()=>{c.remove();await nodeState(n);}))); 
  table(c,['配置阶段','版本','来源'],[
    ['期望配置',s.desired_revision,'主控当前策略'],
    ['节点已接收',s.received_revision,'节点持久化后的接收 ACK'],
    ['节点已应用',s.applied_revision,'derper 应用后的节点 ACK']
  ]);
  if(s.desired_revision>s.applied_revision)c.append(el('p','当前变更待节点应用。已离线的节点在原缓存期限内继续执行旧快照；期限届满由 derper 自行关闭许可。','muted'));
  table(c,['控制器记录','状态 / 时间'],[
    ['控制流',s.control_stream_open?'当前有连接':'当前无连接'],
    ['最后心跳',time(s.node.last_heartbeat)],
    ['域名注册验证',time(s.domain_verified_at)],
    ['最近应用错误',s.node.last_error||'无已记录错误'],
    ['最后应用 ACK 的许可状态',s.reported_usable?'当时有可用许可':'当时没有可用许可'],
    ['当前所示授权的期限',s.policy_has_live_keys?'尚有未到期设备许可':'当前没有未到期设备许可']
  ]);
  table(c,['Tailnet','有效设备数','完整身份刷新','身份缓存截止','主控联系截止','明确授权截止'],s.grants.map(g=>[shortIdentity(g.tailnet_id,names.get(g.tailnet_id)),g.valid_keys,time(g.last_identity_success),time(g.identity_until),time(g.control_until),g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until)]));
  if(s.node.state==='identity_conflict'&&(actor.role==='admin'||n.owner_id===actor.id)){
    c.append(el('p','先在宿主机关闭其他副本，再选择保留的实例。恢复后该实例仍需重新证明私钥。','muted'));
    const f=form(c,'恢复唯一实例',data=>api('/nodes/'+n.id+'/recover','POST',{instance_id:data.get('instance')}));
    field(f,'instance','保留实例 ID','text',true,s.instance_id||'');done(f);
  }
  c.append(el('h2','节点自报运行状态'));
  if(s.report) {
    table(c,['采样时间','控制器接收时间','采样配置版本','采样时许可'],[[time(s.report.observed_at),time(s.reported_at),s.report.revision,s.report.usable?'有可用许可':'没有可用许可']]);
    if(s.report.active_connections!==undefined)c.append(el('p','采样时活动 DERP 连接：'+s.report.active_connections));
    const rates=new Map(s.traffic_rates.map(r=>[r.tailnet_id,r]));
    table(c,['Tailnet','累计 RX / TX 载荷','平均 RX / TX（Mbps）','采样间隔','排队载荷'],s.report.traffic.map(t=>{const r=rates.get(t.tailnet_id);return[shortIdentity(t.tailnet_id,names.get(t.tailnet_id)),actions(bytes(t.rx_payload_bytes),el('span','/'),bytes(t.tx_payload_bytes)),r?(r.rx_bits_per_second/1000000).toFixed(3)+' / '+(r.tx_bits_per_second/1000000).toFixed(3):'等待两次连续计数样本',r?r.interval_seconds.toFixed(1)+' 秒':'—',bytes(t.queued_payload_bytes)];}));
    c.append(el('p','计数来自节点 derper；累计值从当前进程的相应 Tailnet 计数开始。平均值只覆盖最近两次采样之间的 DERP 载荷，不包含协议开销，也不证明对端应用收到数据。','muted'));
  } else c.append(el('p','尚无当前节点实例的运行报告。','muted'));
  c.append(el('h2','主控独立端点探测'));probeTable(c,s.probes,s.node);
  if((actor.role==='admin'||n.owner_id===actor.id)&&['registered','ready','offline'].includes(s.node.state))c.append(button('立即检查端点',async()=>{await api('/nodes/'+n.id+'/probe','POST',{});c.remove();await nodeState(n);}));
  c.scrollIntoView({behavior:'smooth'});
}
async function directory(){
  page('节点列表','为你的 Tailnet 选择 DERP 节点。自己的 Tailnet 使用自己的节点直接生效；使用其他提供者的节点需要对方批准，再由你确认。');
  if(actor.role==='admin')userNames=new Map((await api('/users')).map(u=>[u.id,u.username]));
  const [nodes,tailnets,grants]=await Promise.all([api('/relays'),api('/tailnets'),api('/grants')]);
  const summaries=await Promise.all(nodes.map(n=>api('/nodes/'+n.id+'/summary'))),active=grants.filter(g=>g.state==='active');
  const renderGroup=(title,items)=>{
    const group=el('details',undefined,'card resource-group');group.open=true;
    const heading=el('summary');heading.append(el('strong',title),el('span',items.length+' 个节点','muted'));
    const list=el('div',undefined,'resource-nodes');
    for(const s of items){
      const n=s.node,usable=active.filter(g=>g.node_id===n.id),nodeCard=el('section',undefined,'resource-card node-card'),header=el('div',undefined,'resource-header'),disclosure=el('details',undefined,'resource-disclosure'),summary=el('summary');
      summary.append(el('strong',n.display_name),el('span','提供者 '+s.provider,'muted'),el('span',usable.length+' 个可用 Tailnet','badge'));
      const details=el('div',undefined,'resource-details');
      details.append(el('p',n.domain+' · DERP TCP '+n.derp_port+' / STUN UDP '+n.stun_port,'muted'),actions(status(n.state),el('span','最近心跳 '+time(n.last_heartbeat),'muted')));
      table(details,['Tailnet','授权截止 / 状态'],usable.map(g=>[
        actions(el('span',g.tailnet_name),el('code',g.tailnet_id)),
        actions(status(g.state),el('span',g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until)))
      ]));
      disclosure.append(summary,details);
      const controls=el('div',undefined,'resource-actions');
      controls.append(button('申请使用',()=>request(n,tailnets),'primary'),button('规则与探测',()=>{const c=card(n.display_name+' · 共享信息');ruleSummary(c,s.qos);c.append(el('p','域名注册验证：'+time(s.domain_verified_at)));probeTable(c,s.probes,n);c.scrollIntoView({behavior:'smooth'});}));
      header.append(disclosure,controls);nodeCard.append(header);list.append(nodeCard);
    }
    if(!items.length)list.append(el('p','暂无节点。','muted'));
    group.append(heading,list);$('content').append(group);
  };
  renderGroup('自己的节点',summaries.filter(s=>s.node.owner_id===actor.id));
  renderGroup('其他提供者的节点',summaries.filter(s=>s.node.owner_id!==actor.id));
}
async function request(n,tailnets){if(!tailnets.length)throw new Error('请先绑定自己的 Tailnet。');const c=detailCard('申请使用 '+n.display_name,'request-'+n.id);const f=form(c,'申请 Tailnet 使用此节点',async data=>{const existing=await api('/grants');const g=existing.find(g=>g.node_id===n.id&&g.tailnet_id===data.get('tailnet'));await api('/grants','POST',{node_id:n.id,tailnet_id:data.get('tailnet'),expected_revision:g?.revision||0,explicit_until:data.get('until')?new Date(data.get('until')).toISOString():'0001-01-01T00:00:00Z'});});const choices=tailnets.filter(t=>actor.role==='admin'||t.owner_id===actor.id),input=select(f,'tailnet','使用 Tailnet',[['','请选择 Tailnet'],...choices.map(t=>[t.id,t.display_name+' · 所有者 '+(userNames.get(t.owner_id)||(t.owner_id===actor.id?actor.username:t.owner_id.slice(0,8)))])]);input.required=true;input.value=choices.find(t=>t.owner_id===actor.id)?.id||'';input.addEventListener('change',()=>{let context=c.querySelector('.managed-context');if(context)context.remove();const selected=choices.find(t=>t.id===input.value);if(selected&&selected.owner_id!==actor.id)c.append(el('p',managedLabel(selected.owner_id),'managed-context badge'));});field(f,'until','使用截止时间（可选）','datetime-local',false);done(f);c.scrollIntoView({behavior:'smooth'});}
async function grants(incoming=false){
  page(incoming?'收到的节点使用申请':'节点使用授权',incoming?'处理其他 Tailnet 使用你提供的 DERP 节点的申请。批准后由 Tailnet 所有者确认，设备许可才会生效。':'查看你的 Tailnet 使用哪些 DERP 节点。自己的节点直接生效；其他节点需要提供者批准，再点击“确认使用”。');
  await scopeSelector();const items=(await api('/grants')).filter(g=>(actor.role==='admin'&&resourceScope==='all')||(incoming?g.node_owner_id:g.tailnet_owner_id)===actor.id);
  table(card(incoming?'节点收到的申请':'Tailnet 的节点使用权限'),['DERP 节点','使用的 Tailnet','状态','使用截止 / 版本','操作'],items.map(g=>{
    const owner=incoming&&(actor.role==='admin'||g.node_owner_id===actor.id),applicant=!incoming&&(actor.role==='admin'||g.tailnet_owner_id===actor.id),available=[];
    if(g.state==='requested'){if(owner)available.push(['approve','批准使用'],['reject','拒绝申请']);if(applicant)available.push(['cancel','取消申请']);}
    if(g.state==='owner_approved'){if(applicant)available.push(['confirm','确认使用'],['cancel','取消申请']);if(owner)available.push(['revoke','撤销使用授权']);}
    if(g.state==='active'){if(owner)available.push(['revoke','撤销使用授权']);if(applicant)available.push(['leave','停止使用']);}
    return[actions(el('span',g.node_name),...(actor.role==='admin'&&incoming&&g.node_owner_id!==actor.id?[el('span',managedLabel(g.node_owner_id),'badge')]:[])),actions(el('span',g.tailnet_name),el('span','所有者 '+g.applicant)),status(g.state),(g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until))+' · 版本 '+g.revision,actions(...available.map(([action,label])=>button(label,async()=>{if(['revoke','leave'].includes(action)&&!confirm('确认'+label+'？此 Tailnet 将不能继续使用该 DERP 节点。'))return;await api('/grants/'+g.id+'/actions','POST',{expected_revision:g.revision,action});await render();})),...(['requested','owner_approved','active'].includes(g.state)?[more(button('带宽规则',()=>qos({id:g.node_id,display_name:g.node_name},false)),button('运行状态',()=>nodeState({id:g.node_id,display_name:g.node_name,owner_id:g.node_owner_id})))]:[]))];
  }));
}
async function requests(){return grants(true);}
async function qos(n,editable){
 const [q,state,{tailnets,names}]=await Promise.all([api('/nodes/'+n.id+'/qos'),api('/nodes/'+n.id+'/status'),visibleTailnetNames()]);
 document.getElementById('qos-'+n.id)?.remove();
 const c=detailCard(n.display_name+' · 带宽规则','qos-'+n.id);
 if(actor.role==='admin'&&n.owner_id!==actor.id&&n.owner_id)c.append(el('p',managedLabel(n.owner_id),'badge'));
 c.append(el('p','主控配置预算：'+q.budget_bps/1000000+' Mbps · 当前期望版本 '+state.desired_revision));reportedBudget(c,state);ruleSummary(c,q);
 c.append(el('p','自用组仅允许节点提供者自己的 Tailnet；自用 Tailnet 也可放入共享组。双方都有持续需求时按实际字节竞争，可借用闲置容量，硬上限仍有效。','muted'));
 if(!editable){table(c,['你的 Tailnet','分组','组内权重','硬上限'],q.tailnets.map(r=>[shortIdentity(r.tailnet_id,names.get(r.tailnet_id)),r.group==='owner'?'自用组':'共享组',r.weight,r.max_bps?r.max_bps/1000000+' Mbps':'不另设上限']));return;}
 const own=new Set(tailnets.filter(t=>t.owner_id===n.owner_id).map(t=>t.id));
 const f=form(c,'保存并下发规则',async data=>{
  const body={budget_bps:Math.round(Number(data.get('budget'))*1000000),owner_weight:Number(data.get('owner')),shared_weight:Number(data.get('shared')),shared_max_bps:Math.round(Number(data.get('shared_max')||0)*1000000),tailnets:q.tailnets.map((r,i)=>({tailnet_id:r.tailnet_id,group:data.get('group'+i),weight:Number(data.get('weight'+i)),max_bps:Math.round(Number(data.get('max'+i)||0)*1000000)}))};
  await api('/nodes/'+n.id+'/qos','POST',body);return()=>qos(n,true);
 });f.successMessage='规则已保存，正在等待节点报告应用。';
 for(const [name,label,value]of [['budget','主控总预算（Mbps）',q.budget_bps/1000000],['owner','自用组权重',q.owner_weight],['shared','共享组权重',q.shared_weight],['shared_max','共享组硬上限（Mbps，0 表示不另设上限）',q.shared_max_bps/1000000]]){
  const input=field(f,name,label,'number',true,value);input.min=['owner','shared'].includes(name)?'1':name==='shared_max'?'0':'0.000008';input.step=['owner','shared'].includes(name)?'1':'any';
 }
 const preview=el('p',undefined,'muted');f.append(preview);const update=()=>{const owner=Number(f.elements.owner.value),shared=Number(f.elements.shared.value);preview.textContent=owner+':'+shared+' → 自用 '+(owner/(owner+shared)*100).toFixed(1)+'% / 共享 '+(shared/(owner+shared)*100).toFixed(1)+'%（双方持续有需求时）';};f.elements.owner.addEventListener('input',update);f.elements.shared.addEventListener('input',update);update();
 q.tailnets.forEach((r,i)=>{const h=el('h3');h.append(shortIdentity(r.tailnet_id,names.get(r.tailnet_id)));f.append(h);const group=select(f,'group'+i,'分组',own.has(r.tailnet_id)?[['owner','自用组'],['shared','共享组']]:[['shared','共享组']]);group.value=r.group;const weight=field(f,'weight'+i,'Tailnet 权重','number',true,r.weight);weight.min='1';weight.step='1';const max=field(f,'max'+i,'Tailnet 硬上限（Mbps，0 表示不另设上限）','number',true,r.max_bps/1000000);max.min='0';max.step='any';});done(f);
 if(state.desired_revision!==state.applied_revision)limitedRefresh(c,async()=>{const fresh=await api('/nodes/'+n.id+'/status');const message=c.querySelector('.application-observation')||el('p',undefined,'application-observation');message.textContent=fresh.applied_revision===fresh.desired_revision?'节点已报告应用版本 '+fresh.applied_revision:'等待下发/应用：期望 '+fresh.desired_revision+' · 接收 '+fresh.received_revision+' · 应用 '+fresh.applied_revision;c.append(message);return fresh.desired_revision!==fresh.applied_revision;});
}
async function account(){page('我的账号','密码修改后已有会话会失效，请重新登录。');const f=form(card('修改密码'),'更新密码',async(data,f)=>{const password=data.get('password');f.elements.password.value='';await api('/users/'+actor.id+'/password','POST',{password});await session();});const input=field(f,'password','新密码（12–72 字节）','password');input.autocomplete='new-password';done(f);}
const roleNames={admin:'主控管理员',provider:'节点提供者',member:'成员'};
async function users(){
 page('账号管理','节点提供者可以提供 DERP 节点并管理自己的 Tailnet；成员只管理自己的 Tailnet 和节点使用权限。管理员代操作记录实际管理员身份。');
 const items=await api('/users');
 table(card('主控账号'),['用户名','角色','状态','操作'],items.map(u=>[u.username,roleNames[u.role],u.enabled?'启用':'暂停',actions(...(u.role==='admin'?[]:[button('修改角色',()=>userRole(u))]),more(button(u.enabled?'暂停':'启用',async()=>{await api('/users/'+u.id+'/enabled','POST',{enabled:!u.enabled});await render();}),button('重置密码',()=>password(u))))]));
 const f=form(card('创建账号'),'创建账号',async(data,f)=>{const body={username:data.get('username'),password:data.get('password'),role:data.get('role')};f.elements.password.value='';await api('/users','POST',body);});
 field(f,'username','用户名');select(f,'role','账号角色',[['member','成员'],['provider','节点提供者']]);field(f,'password','初始密码（12–72 字节）','password');done(f);
}
async function userRole(u){const c=card('修改 '+u.username+' 的角色');c.append(el('p','角色修改后此账号需要重新登录。仍拥有节点的提供者需先处理自己的节点，才能改为成员。','muted'));const f=form(c,'保存角色',data=>api('/users/'+u.id+'/role','POST',{role:data.get('role')}));const input=select(f,'role','账号角色',[['member','成员'],['provider','节点提供者']]);input.value=u.role;done(f);c.scrollIntoView({behavior:'smooth'});}
async function password(u){const f=form(card('重置 '+u.username+' 的密码'),'重置密码',async(data,f)=>{const password=data.get('password');f.elements.password.value='';await api('/users/'+u.id+'/password','POST',{password});});field(f,'password','新密码','password');done(f);f.scrollIntoView({behavior:'smooth'});}
async function settings(){
 page('集群设置','身份源和主控联系各有独立的离线保留期。失败或重发快照不会更新最后成功时间。');const q=await api('/settings/retention');const f=form(card('离线保留'),'保存保留期',data=>api('/settings/retention','POST',{identity_seconds:Number(data.get('identity')),control_seconds:Number(data.get('control'))}));
 for(const [name,label,value]of [['identity','身份缓存保留（秒）',q.identity_seconds],['control','主控联系保留（秒）',q.control_seconds]]){const input=field(f,name,label,'number',true,value),hint=el('small',undefined,'muted');input.after(hint);const update=()=>{const seconds=Number(input.value);hint.textContent=seconds%86400===0?seconds/86400+' 天':seconds%3600===0?seconds/3600+' 小时':seconds+' 秒（'+seconds/3600+' 小时）';};input.addEventListener('input',update);update();}done(f);
}
async function events(){
 page('事件与审计','过滤范围为当前返回的最近 200 条可见记录。主控记录保留实际操作者。');
 const [events,audit,nodes,tailnets,grants]=await Promise.all([api('/events'),api('/audit'),api('/nodes'),api('/tailnets'),api('/grants')]);
 const resources=new Map();for(const n of nodes)resources.set('node:'+n.id,{name:n.display_name,href:'nodes'});for(const t of tailnets)resources.set('tailnet:'+t.id,{name:t.display_name,href:'tailnets'});for(const g of grants)resources.set('grant:'+g.id,{name:g.applicant+' 的 '+g.tailnet_name+' 使用 '+g.node_name,href:g.node_owner_id===actor.id?'requests':'grants'});
 const actionNames={'grant.request':'申请使用','grant.approve':'批准使用','grant.confirm':'确认使用','grant.cancel':'取消申请','grant.reject':'拒绝申请','grant.revoke':'撤销使用授权','grant.leave':'停止使用','node.create':'创建节点','node.enroll':'注册节点','node.qos':'更新带宽规则','node.enabled':'修改节点启用状态','node.delete':'删除节点','node.domain.change':'更换节点域名','node.domain.verify':'验证节点域名','node.ports.change':'修改公开端口','node.probe':'检查节点端点','node.recover':'恢复唯一实例','node.identity_conflict':'检测到节点身份冲突','enrollment.issue':'签发注册码','tailnet.create':'绑定 Tailnet','tailnet.enabled':'修改 Tailnet 启用状态','tailnet.delete':'删除 Tailnet','tailnet.transfer':'转移 Tailnet','credential.replace':'替换 OAuth 凭据','credential.delete':'删除 OAuth 凭据','retention.update':'修改保留期','user.create':'创建账号','user.role':'修改账号角色','user.enabled':'修改账号启用状态','user.password':'更新账号密码'};
 const resource=(type,id)=>resources.get(type+':'+id)?.name||({node:'节点',tailnet:'Tailnet',grant:'使用授权',user:'账号',cluster:'集群'}[type]||type)+' '+id.slice(0,8)+'…';
 table(card('站内事件'),['时间','资源','事件','状态'],events.map(e=>[time(e.created_at),shortIdentity(e.resource_id,resource(e.resource_type,e.resource_id)),e.message,e.resolved_at?'结束 / 恢复于 '+time(e.resolved_at):'待处理']));
 const c=card('操作审计'),filters=el('div',undefined,'forms');c.append(filters);const user=field(filters,'actor','操作者筛选','search',false),action=field(filters,'action','动作筛选','search',false),target=field(filters,'resource','资源筛选','search',false),count=el('p',undefined,'muted'),result=el('div');c.append(count,result);
 const update=()=>{const items=audit.filter(a=>(a.actor_username||a.actor_id).toLowerCase().includes(user.value.toLowerCase())&&(actionNames[a.action]||a.action).toLowerCase().includes(action.value.toLowerCase())&&(resource(a.resource_type,a.resource_id)+' '+a.resource_id).toLowerCase().includes(target.value.toLowerCase()));count.textContent='显示 '+items.length+' / '+audit.length+' 条（最近最多 200 条可见记录）';result.replaceChildren();table(result,['时间','实际操作者 / 操作','资源','详情'],items.map(a=>{
  const name=resource(a.resource_type,a.resource_id),item=resources.get(a.resource_type+':'+a.resource_id),r=item?el('a',name):el('span',name);if(item)r.href='#'+item.href;
  const details=el('details',undefined,'identity');details.append(el('summary','代码与完整 ID'),el('code',a.action),el('code',a.resource_id),button('复制详情',()=>copyText(a.action+'\n'+a.resource_id)));
  return [time(a.created_at),(a.actor_username||a.actor_id)+' '+(actionNames[a.action]||a.action),r,details];
 }));};[user,action,target].forEach(input=>input.addEventListener('input',update));update();
}
function localState(s) {
 const c=card('本机运行状态');
 table(c,['项目','状态'],[['角色',s.role==='setup'?'等待初始设置':s.role==='controller'?'主控':'成员节点'],['集群连接',s.joined?(s.control.connected?'已连接主控':'已注册，主控连接中断'):'尚未加入'],['中继许可',s.control.usable?'当前有有效许可':'当前没有有效许可'],['已接收 / 已应用策略',s.control.received_revision+' / '+s.control.applied_revision],['主控下发总预算',s.policy_budget_bps?s.policy_budget_bps/1000000+' Mbps':'尚无策略'],['本地总限速',s.active.max_budget_bps?s.active.max_budget_bps/1000000+' Mbps':'不另设上限'],['当前有效调度预算',s.effective_budget_bps?s.effective_budget_bps/1000000+' Mbps':'尚未应用'],['配置应用',s.pending_apply?'已保存，待应用':'已应用']]);
 if(s.apply_error)c.append(el('p','最近应用未成功，请检查已保存设置与证书后重试。','error'));
 const fields=[['role','角色'],['hostname','公共 DERP 域名'],['derp_listen','DERP TCP 监听'],['stun_listen','STUN UDP 监听'],['tls_mode','TLS 模式'],['cert_mode','证书方式'],['derp_port','公开 DERP TCP 端口'],['stun_port','公开 STUN UDP 端口'],['max_budget_bps','本地总限速（Mbps）'],['logging_level','日志级别']];
 const differences=fields.filter(([key])=>s.saved[key]!==s.active[key]);
 if(differences.length)table(c,['待应用项目','运行中配置','已保存配置'],differences.map(([key,label])=>[label,key==='max_budget_bps'?s.active[key]/1000000+' Mbps':String(s.active[key]),key==='max_budget_bps'?s.saved[key]/1000000+' Mbps':String(s.saved[key])]));
 table(c,['最后联系主控','流量采样时间','活动 DERP 连接'],[[time(s.control.last_contact),time(s.control.traffic_observed_at),s.control.active_connections]]);
 if(s.control.traffic?.length)table(c,['Tailnet','累计 RX / TX 载荷','排队载荷'],s.control.traffic.map(t=>[shortIdentity(t.tailnet_id),actions(bytes(t.rx_payload_bytes),el('span','/'),bytes(t.tx_payload_bytes)),bytes(t.queued_payload_bytes)]));
 return c;
}

function localConfigForm(s,role,initial=false) {
 const c=card(role==='controller'?'主控内置 DERP 节点配置':'DERP 节点本机配置');
 const q=s.saved;
 c.append(el('p','公开端口填写客户端访问的入口；监听地址填写容器或本机的实际地址。端口映射、DNS 和 HTTPS 反代需在宿主机配置。','muted'));
 const read=data=>({role,hostname:data.get('hostname'),derp_listen:data.get('derp_listen'),stun_listen:data.get('stun_listen'),tls_mode:data.get('tls_mode'),cert_mode:data.get('cert_mode'),derp_port:Number(data.get('derp_port')),stun_port:Number(data.get('stun_port')),max_budget_bps:Math.round(Number(data.get('limit'))*1000000),logging_level:data.get('logging_level')});
 const f=form(c,initial?'保存初始设置':'保存配置',async data=>{
  const settings=read(data);settings.hostname=settings.hostname.trim();await api('/local/settings','POST',settings);
  if(initial&&!(settings.tls_mode==='passthrough'&&settings.cert_mode==='manual')){await api('/local/apply','POST',{});await session();return()=>{location.hash=settings.role==='controller'?'member':'member';};}
 });
 f.successMessage='配置已保存；运行中的配置要在应用后才会改变。';
 field(f,'hostname','公共 DERP 域名','text',true,q.hostname);
 field(f,'derp_listen','DERP TCP 监听地址','text',true,q.derp_listen);
 field(f,'stun_listen','STUN UDP 监听地址','text',true,q.stun_listen);
 const tls=select(f,'tls_mode','TLS 模式',[['external','external · HTTPS 由反代终止'],['passthrough','passthrough · 中继提供 TLS']]);tls.value=q.tls_mode;
 const cert=select(f,'cert_mode','证书方式',[['none','由反代提供'],['manual','上传手动证书'],['letsencrypt','Let’s Encrypt']]);cert.value=q.cert_mode;
 tls.addEventListener('change',()=>cert.value=tls.value==='external'?'none':'manual');
 publicPortFields(f,q);
 const limit=field(f,'limit','本地 DERP 总限速（Mbps，0 表示不另设上限）','number',true,q.max_budget_bps/1000000);limit.min='0';limit.step='any';
 c.append(el('p','本地总限速独立保存，主控策略不会覆写它。RX 和 TX 各不超过本地限制与主控预算中的较小值；分组、Tailnet 权重和单独上限继续遵守主控策略。修改后需要应用配置。','muted'));
 const logging=select(f,'logging_level','日志级别',[['info','info'],['warn','warn'],['error','error'],['debug','debug']]);logging.value=q.logging_level;
 done(f);
 const steps=el('p',initial?'手动 TLS：① 保存设置 → ② 保存匹配证书 → ③ 应用设置 → 注册或加入集群。':'保存设置和证书后，点击应用；应用前会检查证书。','steps');c.prepend(steps);
 const certCard=card('② 上传手动证书');
 certCard.id='manual-certificate';
 const certificateNames={not_required:'此模式无需本机手动证书',missing:'证书或私钥尚未保存',invalid:'证书链与私钥无效',hostname_mismatch:'证书与保存域名不匹配',ready:'证书已就绪'};
 certCard.append(el('p',(certificateNames[s.certificate?.state]||'尚无证书状态')+(s.certificate?.state==='ready'?' · '+s.certificate.hostname+' · 到期 '+time(s.certificate.not_after):''),'muted'));
 certCard.append(el('p','上传与已保存域名匹配的 PEM 证书链和私钥，上传后应用配置。独立管理入口的 HTTPS 由宿主机反代提供。','muted'));
 const upload=form(certCard,'保存证书',async(data,f)=>{const certificate=f.elements.certificate.files[0],key=f.elements.private_key.files[0];if(!certificate||!key)throw new Error('请选择证书链和私钥文件。');await api('/local/certificate','POST',{certificate:await certificate.text(),private_key:await key.text()});f.reset();});
 field(upload,'certificate','PEM 证书链','file');field(upload,'private_key','PEM 私钥','file');done(upload);
 upload.successMessage='证书已保存，尚未应用。';
 const apply=button('③ 应用已保存设置',async()=>{await api('/local/apply','POST',{});await session();notice('设置已应用；下一步注册本机节点或加入集群。');location.hash='member';});
 apply.disabled=initial&&s.saved.role==='setup';c.append(apply);
 const updateCertificate=()=>{const manual=tls.value==='passthrough'&&cert.value==='manual';certCard.hidden=!manual;steps.hidden=!manual;f.submitButton.textContent=initial?(manual?'① 保存初始设置':'保存并应用初始设置'):'保存配置';};
 tls.addEventListener('change',updateCertificate);cert.addEventListener('change',updateCertificate);updateCertificate();
}

async function bootstrap(){
 const s=await api('/local/status');
 page('初始设置','选择本机角色。主控与各 DERP 节点使用各自的管理员账号，成员节点与主控失联时仍可登录本机管理面板。');
 const chooser=card('选择角色');
 chooser.append(actions(button('配置为主控',()=>{document.querySelectorAll('#content .card').forEach(c=>{if(c!==chooser)c.remove();});localConfigForm(s,'controller',true);}),button('加入已有集群',()=>{document.querySelectorAll('#content .card').forEach(c=>{if(c!==chooser)c.remove();});localConfigForm(s,'member',true);})));
 if(s.saved.role!=='setup')localConfigForm(s,s.saved.role,true);
 const extra=el('details',undefined,'state-disclosure');extra.append(el('summary','查看本机运行状态'));$('content').append(extra);const state=localState(s);extra.append(state);
}

async function localSettings(){const s=await api('/local/status');page('本机设置','保存配置后单独应用。应用会重启本机中继，管理面板保持可访问。');localState(s);localConfigForm(s,s.role);}

async function member(){
 const s=await api('/local/status'),controller=serverRole==='controller';
 page(controller?'主控内置节点':'本机节点',controller?'管理与主控一起运行的 DERP 节点。注销本机节点会停止其中继与使用许可，主控管理功能继续运行。':'本机管理员管理节点的集群连接与本地配置。共享授权、分组权重及 Tailnet 优先级由节点提供者在主控面板配置。');
 const c=localState(s);
 c.append(button('本机设置',()=>{location.hash='local';}));
 if(s.qos){const rules=card('主控下发规则');ruleSummary(rules,s.qos);table(rules,['Tailnet','分组','权重','硬上限'],s.qos.tailnets.map(t=>[shortIdentity(t.tailnet_id),t.group==='owner'?'自用组':'共享组',t.weight,t.max_bps?t.max_bps/1000000+' Mbps':'不另设上限']));}
 if(s.joined){table(c,['主控地址','集群 ID','节点 ID'],[[s.controller_url,s.cluster_id,s.node_id]]);c.append(button(controller?'注销本机节点':'退出集群',async()=>{if(!confirm(controller?'注销会关闭本机 DERP 连接并清除设备许可，主控继续运行。重新注册后需要重新授权。确认继续？':'退出会关闭本机 DERP 连接并清除设备许可。重新加入需要主控签发新注册码及重新授权。确认继续？'))return;const result=await api('/local/leave','POST',{});await render();notice(controller?'本机节点已注销。':result.released?'本机已退出，主控已释放节点。':'本机已退出；主控暂未确认释放，请在主控重新签发注册码。');},'danger'));}
 else if(controller)c.append(button('注册本机节点',async()=>{await api('/local/register','POST',{display_name:s.saved.hostname||'主控内置节点'});await render();},'primary'));
 else {const f=form(card('加入已有集群'),'验证并加入',async(data,f)=>{const body={controller_url:data.get('controller_url'),enrollment_code:data.get('enrollment_code')};f.elements.enrollment_code.value='';await api('/local/join','POST',body);});field(f,'controller_url','主控 HTTPS 地址','url',true,s.controller_url);const code=field(f,'enrollment_code','一次性注册码','password');code.autocomplete='off';done(f);}
}

async function overview(){
 const localOnly=serverRole==='member',admin=actor.role==='admin';
 const [allNodes,allTailnets,allGrants,local]=await Promise.all([
  localOnly?[]:api(actor.role==='member'?'/relays':'/nodes'),localOnly?[]:api('/tailnets'),localOnly?[]:api('/grants'),admin?api('/local/status'):null
 ]);
 page('概览',localOnly?'本机 DERP 节点的集群连接、带宽与运行状态。':admin?'集群节点、Tailnet 身份源与使用授权，一目了然。':'你的 DERP 资源、Tailnet 身份源与使用授权。');
 if(admin&&!localOnly)await scopeSelector();
 const global=admin&&resourceScope==='all',nodeItems=allNodes.filter(n=>!admin||global||n.owner_id===actor.id),tailnetItems=allTailnets.filter(t=>!admin||global||t.owner_id===actor.id),grantItems=allGrants.filter(g=>!admin||global||g.node_owner_id===actor.id||g.tailnet_owner_id===actor.id);
 const next=card('下一步');for(const action of nextActions({actor,serverRole,local,nodes:allNodes,tailnets:allTailnets,grants:allGrants})){const link=el('a',action.title);link.href='#'+action.href;next.append(actions(el('span',action.path,'badge'),link));}
 const root=el('div',undefined,'overview'),hero=el('section',undefined,'overview-hero'),intro=el('div');
 intro.append(el('p',localOnly?'DERP NODE':'UNIDERP CONTROL','eyebrow'),el('h2',localOnly?(local.active.hostname||'本机 DERP 节点'):'连接你的网络'),el('p',localOnly?'本地限速与主控策略共同约束中继带宽。':'让 DERP 节点与 Tailnet 在同一处保持连接。','muted'),el('span',actor.username+' · '+(localOnly?'节点管理员':roleNames[actor.role]),'badge'));
 const art=el('div',undefined,'overview-network');art.setAttribute('aria-hidden','true');
 art.innerHTML='<svg viewBox="0 0 260 140"><path class="network-line" d="M44 42 130 70 218 32M44 108 130 70 218 110"/><circle class="network-halo" cx="130" cy="70" r="36"/><rect class="network-core" x="108" y="48" width="44" height="44" rx="12"/><path class="network-glyph" d="M121 62h18M121 70h18M121 78h10"/><circle class="network-peer" cx="44" cy="42" r="11"/><circle class="network-peer" cx="44" cy="108" r="11"/><circle class="network-peer" cx="218" cy="32" r="11"/><circle class="network-peer" cx="218" cy="110" r="11"/></svg>';
 hero.append(intro,art);root.append(hero);$('content').append(root);
 const metrics=el('div',undefined,'overview-metrics');root.append(metrics);
 const metric=(title,value,hint,href)=>{const c=el('a',undefined,'overview-metric');c.href='#'+href;c.append(el('span',title,'muted'),el('strong',String(value)),el('small',hint));metrics.append(c);};
 const panel=(title,href,label)=>{const c=el('section',undefined,'overview-panel'),head=el('div',undefined,'row space'),link=el('a',label);link.href='#'+href;head.append(el('h2',title),link);c.append(head);return c;};
 const item=(parent,title,detail,badge,href)=>{const r=el('div',undefined,'overview-item'),text=el('div'),heading=el('strong',title);if(href){const link=el('a');link.href='#'+href;link.append(heading);text.append(link);}else text.append(heading);text.append(el('small',detail));r.append(text);if(badge)r.append(badge);parent.append(r);};
 const budget=value=>value?value/1000000+' Mbps':'尚无策略';
 const localPanel=()=>{
  const c=panel(localOnly?'集群与本机':'主控内置节点','member','管理本机节点 →');
  item(c,local.active.hostname||'尚未设置域名',local.joined?'已注册到集群':'尚未注册到集群',el('span',local.joined?(local.control.connected?'已连接主控':'主控连接中断'):'尚未注册','badge '+(local.control.connected?'good':'warn')));
  item(c,'中继许可',local.control.usable?'当前有有效设备许可':'当前没有有效设备许可',status(local.control.usable?'valid':'missing'));
  item(c,'策略版本','接收 '+local.control.received_revision+' / 应用 '+local.control.applied_revision);
  item(c,'最后联系主控',time(local.control.last_contact));
  if(local.apply_error)c.append(el('p','本机设置尚未应用成功，请检查设置。','error'));return c;
 };
 const grid=el('div',undefined,'overview-panels');root.append(grid);
 if(localOnly){
  metric('有效调度预算',budget(local.effective_budget_bps),'RX、TX 各自的总预算','member');
  metric('本地总限速',local.active.max_budget_bps?budget(local.active.max_budget_bps):'不另设上限','独立于主控下发策略','local');
  metric('主控下发预算',budget(local.policy_budget_bps),'在本地限速内生效','member');
  metric('活动 DERP 连接',local.control.active_connections,'采样于 '+time(local.control.traffic_observed_at),'member');
  grid.append(localPanel());const c=panel('节点配置','local','本机设置 →');
  item(c,'TLS 模式',local.active.tls_mode);item(c,'公开端口','DERP TCP '+local.active.derp_port+' · STUN UDP '+local.active.stun_port);
  item(c,'主控地址',local.controller_url||'尚未配置');item(c,'配置应用',local.pending_apply?'已保存，待应用':local.apply_error?'应用失败':'已应用');grid.append(c);
 }else{
  const pending=grantItems.filter(g=>g.state==='requested'||g.state==='owner_approved');
  metric(actor.role==='member'?'可选 DERP 节点':admin?'集群 DERP 节点':'我的 DERP 节点',nodeItems.length,nodeItems.filter(n=>n.enabled).length+' 个已启用',actor.role==='member'?'directory':'nodes');
  metric(admin?'Tailnet 身份源':'我的 Tailnet',tailnetItems.length,tailnetItems.filter(t=>t.enabled&&t.credential_status==='valid').length+' 个已启用且凭据正常','tailnets');
  metric(global?'全局活跃使用授权':'我的活跃使用授权',grantItems.filter(g=>g.state==='active'&&(global||g.tailnet_owner_id===actor.id)).length,global?'集群共享节点的使用关系':'我的 Tailnet 使用共享节点','grants');
  if(actor.role==='provider')metric('等待我批准',grantItems.filter(g=>g.state==='requested'&&g.node_owner_id===actor.id).length,'我的节点收到的申请','requests');
  metric(global?'全局待确认授权':'等待我确认',pending.filter(g=>g.state==='owner_approved'&&(global||g.tailnet_owner_id===actor.id)).length,'提供者已批准，等待使用方确认','grants');
  const nodesPanel=panel(actor.role==='member'?'可选 DERP 节点':admin?'集群节点':'我的节点',actor.role==='member'?'directory':'nodes','查看节点 →');
  for(const n of nodeItems.slice(0,4))item(nodesPanel,n.display_name,n.domain+' · 最近心跳 '+time(n.last_heartbeat),status(n.enabled?n.state:'paused'));
  if(!nodeItems.length)nodesPanel.append(el('p','暂无节点。','muted'));grid.append(nodesPanel);
  const tailnetPanel=panel(admin?'Tailnet 身份源':'我的 Tailnet','tailnets','管理 Tailnet →');
  for(const t of tailnetItems.slice(0,4))item(tailnetPanel,t.display_name,'完整身份成功：'+time(t.last_identity_success),status(t.enabled?t.credential_status:'paused'));
  if(!tailnetItems.length)tailnetPanel.append(el('p','绑定 Tailnet 后，即可为其选择 DERP 节点。','muted'));grid.append(tailnetPanel);
  const grantsPanel=panel('使用授权','grants','查看授权 →');
  for(const g of [...pending,...grantItems.filter(g=>g.state==='active')].slice(0,4))item(grantsPanel,g.tailnet_name+' → '+g.node_name,'授权版本 '+g.revision,status(g.state),g.node_owner_id===actor.id&&g.tailnet_owner_id!==actor.id?'requests':'grants');
  if(!pending.length&&!grantItems.some(g=>g.state==='active'))grantsPanel.append(el('p','暂无待处理或活跃的共享授权。','muted'));grid.append(grantsPanel);
  if(local)grid.append(localPanel());
 }
 const foot=el('div',undefined,'overview-foot row space');foot.append(el('small','数据读取于 '+new Date().toLocaleString()+' · 状态来自主控记录与本机观测','muted'),button('刷新概览',render));root.append(foot);
}

function positionNavMarker(){
 const links=document.querySelector('.navigation-links'),marker=links.querySelector('.nav-marker'),active=links.querySelector('a[aria-current="page"]');
 const group=active?.closest('[data-nav-group]');
 if(!group||!group.open){marker.hidden=true;return;}
 const holder=group.querySelector('summary'),target=active.getBoundingClientRect().top-holder.getBoundingClientRect().top+8;
 if(marker.parentElement!==holder||marker.hidden){
  const from=marker.hidden?target:marker.getBoundingClientRect().top-holder.getBoundingClientRect().top;
  marker.style.transition='none';holder.append(marker);marker.style.transform='translateY('+from+'px)';marker.hidden=false;marker.getBoundingClientRect();marker.style.transition='';
 }
 marker.style.transform='translateY('+target+'px)';
}

const views={overview,tailnets,nodes,directory,grants,requests,account,users,settings,events,bootstrap,local:localSettings,member};
async function render(options={}){
 if(!actor)return false;
 const generation=++pageGeneration,scrollY=window.scrollY;clearTimeout(refreshTimer);
 let name=location.hash.slice(1),allowed;
 if(serverRole==='setup')allowed=actor.role==='admin'?['bootstrap','account']:['account'];
 else if(serverRole==='member')allowed=actor.role==='admin'?['overview','member','local','account']:['account'];
 else {
  allowed=['overview','tailnets','directory','grants','account'];
  if(actor.role==='admin'||actor.role==='provider')allowed.push('nodes','requests');
  if(actor.role==='admin')allowed.push('member','local','users','settings','events');
 }
 if(!allowed.includes(name))name=allowed[0];
 if(location.hash!=='#'+name)history.replaceState(null,'','#'+name);
 const changed=currentPage!==name;currentPage=name;
 $('content').classList.toggle('wide',['overview','tailnets','nodes','directory','grants','requests','users','events'].includes(name));
 document.querySelectorAll('.navigation-links a,#account-menu a').forEach(a=>{a.hidden=!allowed.includes(a.hash.slice(1));if(a.hash==='#nodes')a.textContent=actor.role==='admin'?'节点管理':'我的节点';a.setAttribute('aria-current',a.hash==='#'+name?'page':'false');});
 document.querySelectorAll('[data-nav-group]').forEach(group=>{group.hidden=!Array.from(group.querySelectorAll('a')).some(a=>!a.hidden);if(group.querySelector('a[aria-current="page"]'))group.open=true;});
 positionNavMarker();
 document.body.classList.remove('nav-open');$('nav-toggle').setAttribute('aria-expanded','false');
 try{await views[name]();if(generation!==pageGeneration)return;if(serverRole==='controller'&&(actor.role==='admin'||actor.role==='provider')){const grants=await api('/grants');const link=document.querySelector('a[href="#requests"]'),count=grants.filter(g=>g.node_owner_id===actor.id&&g.state==='requested').length;link.textContent='收到的使用申请'+(count?' · '+count:'');}if(options.preserveScroll&&!changed)window.scrollTo(0,scrollY);else window.scrollTo(0,0);if(options.focusResource)document.getElementById(options.focusResource)?.focus();return generation===pageGeneration;}catch(e){if(e.name==='AbortError')return;notice(e.message,true);if(e.status===401)await session();}
}
async function session(){try{const result=await api('/session');actor=result.actor;csrf=result.csrf_token;serverRole=result.server_role;}catch(e){actor=csrf=serverRole=undefined;}const logged=!!actor;let needsSetup=false;if(!logged){try{needsSetup=(await api('/setup')).required;}catch(e){notice(e.message,true);}}document.body.classList.toggle('logged-in',logged);$('setup').hidden=logged||!needsSetup;$('login').hidden=logged||needsSetup;$('workspace').hidden=!logged;$('account').hidden=!logged;if(!logged)$('content').replaceChildren();if(logged){$('account-avatar').textContent=actor.username.slice(0,2).toUpperCase();$('account-name').textContent=actor.username;$('account-role').textContent=actor.role==='admin'&&serverRole!=='controller'?'节点管理员':roleNames[actor.role];await render();}}
$('setup-form').addEventListener('submit',async e=>{e.preventDefault();const f=e.currentTarget;const data=new FormData(f);const body={username:data.get('username'),password:data.get('password')};if(body.password!==data.get('password_confirm')){notice('两次输入的密码不一致',true);return;}f.elements.password.value=f.elements.password_confirm.value='';const b=f.querySelector('button');b.disabled=true;try{await api('/setup','POST',body);await session();}catch(err){notice(err.message,true);if(err.status===409)await session();}finally{b.disabled=false;}});
$('login-form').addEventListener('submit',async e=>{e.preventDefault();const f=e.currentTarget;const data=new FormData(f);const body={username:data.get('username'),password:data.get('password')};f.elements.password.value='';const b=f.querySelector('button');b.disabled=true;try{await api('/login','POST',body);await session();}catch(err){notice(err.message,true);}finally{b.disabled=false;}});
$('logout').addEventListener('click',async()=>{try{await api('/logout','POST',{});++pageGeneration;clearTimeout(refreshTimer);actor=csrf=undefined;$('content').replaceChildren();await session();}catch(e){notice(e.message,true);}});
$('nav-toggle').addEventListener('click',()=>{const expanded=document.body.classList.toggle('nav-open');$('nav-toggle').setAttribute('aria-expanded',String(expanded));});
configureMenu($('account-toggle'),$('account-menu'));
document.querySelectorAll('[data-theme-choice]').forEach(choice=>choice.addEventListener('click',()=>setTheme(choice.dataset.themeChoice)));
applyTheme();
document.addEventListener('click',e=>{if(document.body.classList.contains('nav-open')&&!e.target.closest('#navigation,#nav-toggle')){document.body.classList.remove('nav-open');$('nav-toggle').setAttribute('aria-expanded','false');}});
window.addEventListener('hashchange',()=>render());
document.addEventListener('visibilitychange',()=>{if(document.hidden)clearTimeout(refreshTimer);});
window.addEventListener('resize',positionNavMarker);
document.querySelectorAll('[data-nav-group]').forEach(group=>{group.addEventListener('toggle',positionNavMarker);group.addEventListener('transitionend',positionNavMarker);});
session();
