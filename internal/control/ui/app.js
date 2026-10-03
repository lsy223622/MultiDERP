'use strict';
let actor, csrf;
const $ = id => document.getElementById(id);
function notice(message, error = false) { const n = $('notice'); n.textContent = message; n.classList.toggle('error',error); n.hidden = false; clearTimeout(notice.timer); notice.timer = setTimeout(() => n.hidden = true,7000); }
async function api(path,method='GET',body) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && csrf) headers['X-CSRF-Token'] = csrf;
  const r = await fetch('/api/v1'+path,{method,headers,credentials:'same-origin',body:body===undefined?undefined:JSON.stringify(body)});
  const result = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(({400:'输入不符合要求，请检查字段。',401:'请重新登录。',403:'你没有执行此操作的权限。',409:'状态已改变，请刷新后重试。',429:'请求过于频繁，请稍后再试。',503:'身份源暂不可用。请核对 Client ID、Secret、只读设备权限和主控的 API 网络，再重试。'})[r.status] || '请求失败，请检查资源状态后重试。'); e.status=r.status; throw e; }
  return result;
}
function el(tag,text,className) { const e=document.createElement(tag); if(text!==undefined)e.textContent=text; if(className)e.className=className; return e; }
function button(label,run,className='quiet') { const b=el('button',label,className); b.type='button'; b.addEventListener('click',async()=>{b.disabled=true;try{await run();}catch(e){notice(e.message,true);}finally{b.disabled=false;}});return b; }
function page(title,hint) { $('content').replaceChildren(el('h1',title),el('p',hint,'muted')); }
function card(title,parent=$('content')) { const c=el('section',undefined,'card');c.append(el('h2',title));parent.append(c);return c; }
function field(form,name,label,type='text',required=true,value='') { const l=el('label',label);const input=el('input');input.name=name;input.type=type;input.required=required;input.value=value;l.append(input);form.append(l);return input; }
function select(form,name,label,items) { const l=el('label',label);const input=el('select');input.name=name;for(const [value,text] of items){const o=el('option',text);o.value=value;input.append(o);}l.append(input);form.append(l);return input; }
function form(parent,submit,run) { const f=el('form');parent.append(f);const b=el('button',submit);b.type='submit';f.addEventListener('submit',async e=>{e.preventDefault();b.disabled=true;try{const after=await run(new FormData(f),f);notice('操作已保存。');await render();if(typeof after==='function')after();}catch(err){notice(err.message,true);}finally{b.disabled=false;}});f.submitButton=b;return f; }
function done(f){f.append(f.submitButton);return f;}
function table(parent,head,rows) { const wrap=el('div',undefined,'table-wrap'),t=el('table'),tr=el('tr');for(const text of head)tr.append(el('th',text));const h=el('thead');h.append(tr);t.append(h);const body=el('tbody');for(const row of rows){const r=el('tr');for(const value of row){const c=el('td');c.append(value instanceof Node?value:el('span',value));r.append(c);}body.append(r);}t.append(body);wrap.append(t);parent.append(wrap);if(!rows.length)parent.append(el('p','暂无记录。','muted')); }
function actions(...items){const r=el('div',undefined,'row');r.append(...items);return r;}
function time(value){if(!value||value==='0001-01-01T00:00:00Z')return '尚无观测';return new Date(typeof value==='number'?value*1000:value).toLocaleString();}
function status(value){const names={pending:'待注册',domain_pending:'等待新域名验证',registered:'已注册',ready:'已报告可用',offline:'离线',identity_conflict:'身份冲突',requested:'待提供者批准',owner_approved:'待申请方确认',active:'活跃',revoked:'已撤销',left:'已退出',rejected:'已拒绝',cancelled:'已取消',expired:'已到期',valid:'正常',invalid:'凭据异常',missing:'未配置',paused:'已暂停',unavailable:'身份源暂不可用'};return el('span',names[value]||value||'尚无状态','badge '+(['ready','active','valid'].includes(value)?'good':['identity_conflict','invalid','revoked'].includes(value)?'bad':'warn'));}
function secretFields(f){field(f,'client_id','OAuth Client ID');return field(f,'client_secret','只读 OAuth Client Secret','password');}
async function tailnets(){
  page('我的尾网','使用稳定的 Tailnet ID 绑定身份源。OAuth 只请求设备只读权限，原始客户端权限由所有者配置。');
  const [items,retention]=await Promise.all([api('/tailnets'),api('/settings/retention')]);
  table(card('已绑定尾网'),['名称 / ID','资源 / 凭据','完整身份成功 / 缓存截止','操作'],items.map(t=>[
    actions(el('span',t.display_name),el('code',t.api_id)),actions(status(t.enabled?'active':'paused'),status(t.credential_status)),
    actions(el('span',time(t.last_identity_success)),el('span',t.last_identity_success?'缓存至 '+time(t.last_identity_success+retention.identity_seconds):'尚无身份缓存')),
    actions(button('替换凭据',()=>credential(t)),button(t.enabled?'暂停':'启用',async()=>{await api('/tailnets/'+t.id+'/enabled','POST',{enabled:!t.enabled});await render();}),button('删除凭据',async()=>{if(!confirm('删除凭据会撤销设备许可，确认继续？'))return;await api('/tailnets/'+t.id+'/credential','DELETE');await render();},'danger'),button('删除尾网',async()=>{if(!confirm('删除尾网会终止所有相关授权。已离线节点的旧许可受原缓存期限约束。确认继续？'))return;await api('/tailnets/'+t.id,'DELETE');await render();},'danger'),...(actor.role==='admin'?[button('转移',()=>transfer(t))]:[]))
  ]));
  const f=form(card('绑定尾网'),'验证并绑定',async(data,f)=>{const body={display_name:data.get('name'),api_id:data.get('api_id'),credential:{client_id:data.get('client_id'),client_secret:data.get('client_secret')}};f.elements.client_secret.value='';await api('/tailnets','POST',body);});
  field(f,'name','显示名称');field(f,'api_id','Tailnet ID（General Settings 中的 T… 标识）');secretFields(f);done(f);
}
async function transfer(t){
  const users=await api('/users'),c=card('转移 '+t.display_name);
  const f=form(c,'转移管理权',data=>api('/tailnets/'+t.id+'/transfer','POST',{owner_id:data.get('owner')}));
  select(f,'owner','新所有者',users.filter(u=>u.enabled).map(u=>[u.id,u.username]));done(f);
}
async function credential(t){const c=card('替换 '+t.display_name+' 的凭据');const f=form(c,'验证并替换',async(data,f)=>{const body={client_id:data.get('client_id'),client_secret:data.get('client_secret')};f.elements.client_secret.value='';await api('/tailnets/'+t.id+'/credential','POST',body);});secretFields(f);done(f);c.scrollIntoView({behavior:'smooth'});}
async function nodes(){
  page('我的服务器','注册后仍需接收并应用策略。暂停会发布空许可；删除和域名变更后，需要使用者更新自己的 DERP map。');
  const items=await api('/nodes');
  table(card('服务器'),['名称','域名','状态','最近心跳','操作'],items.map(n=>[
    n.display_name,el('code',n.domain),actions(status(n.state),...(!n.enabled?[status('paused')]:[])),time(n.last_heartbeat),
    actions(button('状态',()=>nodeState(n)),...(n.state!=='pending'?[button('规则',()=>qos(n,true))]:[button('注册码',()=>enrollment(n))]),button(n.enabled?'暂停':'启用',async()=>{await api('/nodes/'+n.id+'/enabled','POST',{enabled:!n.enabled});await render();}),...(n.state!=='pending'?[button('域名',()=>changeDomain(n))]:[]),button('删除',()=>deleteNode(n),'danger'))
  ]));
  const f=form(card('添加服务器'),'创建并签发注册码',async data=>{const result=await api('/nodes','POST',{display_name:data.get('name'),domain:data.get('domain')});return()=>showEnrollment(result.enrollment);});field(f,'name','显示名称');field(f,'domain','公共 DERP 域名');done(f);
}
async function changeDomain(n){
  const c=card(n.display_name+' · 更换域名');
  c.append(el('p','先在节点宿主机配置新域名的 DNS、TLS 和转发入口。提交时会暂停许可，节点随后以原私钥证明新域名；完成验证后仍需手动启用服务器。使用者需要更新 DERP map。','muted'));
  const f=form(c,'暂停并验证新域名',async data=>{
    await api('/nodes/'+n.id+'/enabled','POST',{enabled:false});
    const state=await api('/nodes/'+n.id+'/status');
    if(state.control_stream_open && state.applied_revision!==state.desired_revision)throw new Error('已暂停，正在等待空许可应用 ACK。请刷新状态后再提交域名。');
    await api('/nodes/'+n.id+'/domain','POST',{domain:data.get('domain')});
  });field(f,'domain','新公共 DERP 域名','text',true,n.domain);done(f);
}
async function deleteNode(n){
  if(!confirm('删除服务器会撤销集群身份和注册码，并移出 DERP map。离线许可到原缓存期限结束。确认继续？'))return;
  if(n.state!=='pending'){
    await api('/nodes/'+n.id+'/enabled','POST',{enabled:false});
    const state=await api('/nodes/'+n.id+'/status');
    if(state.control_stream_open && state.applied_revision!==state.desired_revision){await render();notice('服务器已暂停，正在等待空许可应用 ACK。请刷新状态后再删除。',true);return;}
  }
  await api('/nodes/'+n.id,'DELETE');await render();
}
function showEnrollment(e){const c=card('一次性注册码');c.append(el('p','有效至 '+time(e.expires_at)),el('pre',e.code));c.append(button('关闭并清除',()=>c.remove()));c.scrollIntoView({behavior:'smooth'});}
async function enrollment(n){const e=await api('/nodes/'+n.id+'/enrollment','POST',{});showEnrollment(e);}
function probeTable(parent,probes) {
  table(parent,['主控独立探测','结果','检查时间'],['derp','stun'].map(name=>[name==='derp'?'HTTPS DERP 端点':'UDP STUN 端点',probes?.[name]?.state==='ok'?'成功':probes?.[name]?.state==='failed'?'失败':'尚未检查',time(probes?.[name]?.observed_at)]));
  parent.append(el('p','探测仅表示主控当次能访问该端点。客户端是否能从所在网络连接，需要实际验证。','muted'));
}
function ruleSummary(parent,q) {
  const total=q.owner_weight+q.shared_weight;
  table(parent,['预算 / 组','争用时份额','硬上限'],[
    ['RX / TX 各 '+(q.budget_bps/1000000)+' Mbps','按实际字节计量','各方向受总预算限制'],
    ['owner 组',(q.owner_weight/total*100).toFixed(1)+'%（权重 '+q.owner_weight+'）','总预算'],
    ['shared 组',(q.shared_weight/total*100).toFixed(1)+'%（权重 '+q.shared_weight+'）',q.shared_max_bps?(q.shared_max_bps/1000000)+' Mbps':'未另设上限']
  ]);
  parent.append(el('p','份额用于双方均有需求时的分配；需求不足的一组会让出闲置容量，硬上限持续有效。组内尾网按权重分配，增加共享连接不会增加份额。','muted'));
}
async function nodeState(n) {
  const s=await api('/nodes/'+n.id+'/status'),c=card(n.display_name+' · 观测状态');
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
  table(c,['尾网','有效设备数','完整身份刷新','身份缓存截止','主控联系截止','明确授权截止'],s.grants.map(g=>[g.tailnet_id,g.valid_keys,time(g.last_identity_success),time(g.identity_until),time(g.control_until),g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until)]));
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
    table(c,['尾网','累计 RX / TX 载荷（字节）','平均 RX / TX（Mbps）','采样间隔','排队载荷（字节）'],s.report.traffic.map(t=>{const r=rates.get(t.tailnet_id);return[t.tailnet_id,t.rx_payload_bytes+' / '+t.tx_payload_bytes,r?(r.rx_bits_per_second/1000000).toFixed(3)+' / '+(r.tx_bits_per_second/1000000).toFixed(3):'等待两次连续计数样本',r?r.interval_seconds.toFixed(1)+' 秒':'—',t.queued_payload_bytes];}));
    c.append(el('p','计数来自节点 derper；累计值从当前进程的相应尾网计数开始。平均值只覆盖最近两次采样之间的 DERP 载荷，不包含协议开销，也不证明对端应用收到数据。','muted'));
  } else c.append(el('p','尚无当前节点实例的运行报告。','muted'));
  c.append(el('h2','主控独立端点探测'));probeTable(c,s.probes);
  if((actor.role==='admin'||n.owner_id===actor.id)&&['registered','ready','offline'].includes(s.node.state))c.append(button('立即检查端点',async()=>{await api('/nodes/'+n.id+'/probe','POST',{});c.remove();await nodeState(n);}));
  c.scrollIntoView({behavior:'smooth'});
}
async function directory(){
  page('服务器目录','选择你的尾网申请共享。提供者批准后，需要申请方确认，授权才会生效。');
  const [nodes,tailnets]=await Promise.all([api('/relays'),api('/tailnets')]);
  const summaries=await Promise.all(nodes.map(n=>api('/nodes/'+n.id+'/summary')));
  table(card('可申请服务器'),['服务器 / 提供者','域名','最近心跳','操作'],summaries.map(s=>[actions(el('span',s.node.display_name),el('span','提供者 '+s.provider)),el('code',s.node.domain),actions(status(s.node.state),el('span',time(s.node.last_heartbeat))),actions(button('申请',()=>request(s.node,tailnets)),button('规则与探测',()=>{const c=card(s.node.display_name+' · 共享信息');ruleSummary(c,s.qos);c.append(el('p','域名注册验证：'+time(s.domain_verified_at)));probeTable(c,s.probes);c.scrollIntoView({behavior:'smooth'});}))]));
}
async function request(n,tailnets){if(!tailnets.length)throw new Error('请先绑定自己的尾网。');const c=card('申请 '+n.display_name);const f=form(c,'提交申请',async data=>{const existing=await api('/grants');const g=existing.find(g=>g.node_id===n.id&&g.tailnet_id===data.get('tailnet'));await api('/grants','POST',{node_id:n.id,tailnet_id:data.get('tailnet'),expected_revision:g?.revision||0,explicit_until:data.get('until')?new Date(data.get('until')).toISOString():'0001-01-01T00:00:00Z'});});select(f,'tailnet','使用尾网',tailnets.filter(t=>t.owner_id===actor.id).map(t=>[t.id,t.display_name]));field(f,'until','明确截止（可选）','datetime-local',false);done(f);c.scrollIntoView({behavior:'smooth'});}
async function grants(){page('申请与授权','申请 → 提供者批准 → 申请方确认。撤销后重新申请会产生新版本，凭据恢复不会重新激活旧授权。');const items=await api('/grants');table(card('授权关系'),['服务器','尾网','状态','截止 / 版本','操作'],items.map(g=>{const owner=actor.role==='admin'||g.node_owner_id===actor.id,applicant=actor.role==='admin'||g.tailnet_owner_id===actor.id;const available=[];if(g.state==='requested'){if(owner)available.push(['approve','批准'],['reject','拒绝']);if(applicant)available.push(['cancel','取消']);}if(g.state==='owner_approved'){if(applicant)available.push(['confirm','确认'],['cancel','取消']);if(owner)available.push(['revoke','撤销']);}if(g.state==='active'){if(owner)available.push(['revoke','撤销']);if(applicant)available.push(['leave','退出']);}return[g.node_name,actions(el('span',g.tailnet_name),el('span','申请者 '+g.applicant)),status(g.state),(g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until))+' · r'+g.revision,actions(...available.map(([action,label])=>button(label,async()=>{if(['revoke','leave'].includes(action)&&!confirm('确认'+label+'此授权？'))return;await api('/grants/'+g.id+'/actions','POST',{expected_revision:g.revision,action});await render();})),...(['requested','owner_approved','active'].includes(g.state)?[button('当前规则',()=>qos({id:g.node_id,display_name:g.node_name},false)),button('观测状态',()=>nodeState({id:g.node_id,display_name:g.node_name,owner_id:g.node_owner_id}))]:[]))];}));}
async function qos(n,editable){const q=await api('/nodes/'+n.id+'/qos');const c=card(n.display_name+' · 带宽规则');c.append(el('p','争用时按字节分配组份额；需求不足时可借用闲置份额，硬上限仍有效。RX 和 TX 各使用同一预算。','muted'));ruleSummary(c,q);if(!editable){table(c,['你的尾网','分组','组内权重','硬上限'],q.tailnets.map(r=>[r.tailnet_id,r.group,r.weight,r.max_bps?(r.max_bps/1000000)+' Mbps':'未另设上限']));return;}const tailnets=await api('/tailnets');const own=new Set(tailnets.filter(t=>t.owner_id===n.owner_id).map(t=>t.id));const f=form(c,'热应用规则',async(data)=>{const body={budget_bps:Math.round(Number(data.get('budget'))*1000000),owner_weight:Number(data.get('owner')),shared_weight:Number(data.get('shared')),shared_max_bps:Math.round(Number(data.get('shared_max')||0)*1000000),tailnets:q.tailnets.map((r,i)=>({tailnet_id:r.tailnet_id,group:data.get('group'+i),weight:Number(data.get('weight'+i)),max_bps:Math.round(Number(data.get('max'+i)||0)*1000000)}))};await api('/nodes/'+n.id+'/qos','POST',body);});for(const [name,label,value]of[['budget','总预算（Mbps）',q.budget_bps/1000000],['owner','owner 权重',q.owner_weight],['shared','shared 权重',q.shared_weight],['shared_max','shared 硬上限（Mbps，0 表示不单设上限）',q.shared_max_bps/1000000]]){const input=field(f,name,label,'number',true,value);input.min=['owner','shared'].includes(name)?'1':name==='shared_max'?'0':'0.000008';input.step=['owner','shared'].includes(name)?'1':'any';}q.tailnets.forEach((r,i)=>{f.append(el('h2',r.tailnet_id));const group=select(f,'group'+i,'分组',own.has(r.tailnet_id)?[['owner','owner'],['shared','shared']]:[['shared','shared']]);group.value=r.group;field(f,'weight'+i,'尾网权重','number',true,r.weight);field(f,'max'+i,'尾网硬上限（Mbps，0 表示不单设上限）','number',true,r.max_bps/1000000);});done(f);c.scrollIntoView({behavior:'smooth'});}
async function map(){page('DERP map','生成标准 DERPMap 片段，并自行合并到尾网策略。保留已有 Regions 和默认 region；这项操作不会写入尾网配置。');const items=await api('/tailnets');const c=card('选择尾网');select(c,'tailnet','尾网',items.map(t=>[t.id,t.display_name]));c.append(button('生成片段',async()=>{const result=await api('/tailnets/'+c.querySelector('select').value+'/derpmap');let output=c.querySelector('textarea');if(!output){output=el('textarea');output.readOnly=true;output.setAttribute('aria-label','DERPMap JSON');c.append(output);}output.value=JSON.stringify(result,null,2);}));}
async function account(){page('我的账号','密码修改后已有会话会失效，请重新登录。');const f=form(card('修改密码'),'更新密码',async(data,f)=>{const password=data.get('password');f.elements.password.value='';await api('/users/'+actor.id+'/password','POST',{password});await session();});const input=field(f,'password','新密码（12–72 字节）','password');input.autocomplete='new-password';done(f);}
async function users(){page('账号管理','管理员代操作始终记录当前管理员身份。');const items=await api('/users');table(card('集群账号'),['用户名','角色','状态','操作'],items.map(u=>[u.username,u.role,u.enabled?'启用':'暂停',actions(button(u.enabled?'暂停':'启用',async()=>{await api('/users/'+u.id+'/enabled','POST',{enabled:!u.enabled});await render();}),button('重置密码',()=>password(u)))]));const f=form(card('创建成员'),'创建账号',async(data,f)=>{const body={username:data.get('username'),password:data.get('password')};f.elements.password.value='';await api('/users','POST',body);});field(f,'username','用户名');field(f,'password','初始密码（12–72 字节）','password');done(f);}
async function password(u){const f=form(card('重置 '+u.username+' 的密码'),'重置密码',async(data,f)=>{const password=data.get('password');f.elements.password.value='';await api('/users/'+u.id+'/password','POST',{password});});field(f,'password','新密码','password');done(f);f.scrollIntoView({behavior:'smooth'});}
async function settings(){page('集群设置','身份源和主控联系各有独立的离线保留期。失败或重发快照不会更新最后成功时间。');const q=await api('/settings/retention');const f=form(card('离线保留'),'保存保留期',data=>api('/settings/retention','POST',{identity_seconds:Number(data.get('identity')),control_seconds:Number(data.get('control'))}));field(f,'identity','身份缓存保留（秒）','number',true,q.identity_seconds);field(f,'control','主控联系保留（秒）','number',true,q.control_seconds);done(f);}
async function events(){page('事件与审计','各展示最近 200 条可见记录；已恢复的连续故障保留恢复时间。');const [events,audit]=await Promise.all([api('/events'),api('/audit')]);table(card('站内事件'),['时间','资源','事件','状态'],events.map(e=>[time(e.created_at),e.resource_id,e.message,e.resolved_at?'结束 / 恢复于 '+time(e.resolved_at):'待处理']));table(card('操作审计'),['时间','实际操作者','动作','资源'],audit.map(a=>[time(a.created_at),a.actor_username||a.actor_id,a.action,a.resource_id]));}
const views={tailnets,nodes,directory,grants,map,account,users,settings,events};
async function render(){if(!actor)return;const name=location.hash.slice(1)||'tailnets';document.querySelectorAll('nav a').forEach(a=>a.setAttribute('aria-current',a.hash==='#'+name?'page':'false'));try{await (views[name]||tailnets)();}catch(e){notice(e.message,true);if(e.status===401)await session();}}
async function session(){try{const result=await api('/session');actor=result.actor;csrf=result.csrf_token;}catch(e){actor=csrf=undefined;}const logged=!!actor;$('login').hidden=logged;$('workspace').hidden=!logged;$('account').hidden=!logged;if(!logged)$('content').replaceChildren();if(logged){$('account-name').textContent=actor.username;document.querySelectorAll('[data-admin]').forEach(e=>e.hidden=actor.role!=='admin');await render();}}
$('login-form').addEventListener('submit',async e=>{e.preventDefault();const f=e.currentTarget;const data=new FormData(f);const body={username:data.get('username'),password:data.get('password')};f.elements.password.value='';const b=f.querySelector('button');b.disabled=true;try{await api('/login','POST',body);await session();}catch(err){notice(err.message,true);}finally{b.disabled=false;}});
$('logout').addEventListener('click',async()=>{try{await api('/logout','POST',{});actor=csrf=undefined;$('content').replaceChildren();await session();}catch(e){notice(e.message,true);}});
window.addEventListener('hashchange',render);
session();

