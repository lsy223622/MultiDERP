'use strict';
let actor, csrf, serverRole;
const $ = id => document.getElementById(id);
function notice(message, error = false) { const n = $('notice'); n.textContent = message; n.classList.toggle('error',error); n.hidden = false; clearTimeout(notice.timer); notice.timer = setTimeout(() => n.hidden = true,7000); }
async function api(path,method='GET',body) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && csrf) headers['X-CSRF-Token'] = csrf;
  const r = await fetch('/api/v1'+path,{method,headers,credentials:'same-origin',body:body===undefined?undefined:JSON.stringify(body)});
  const result = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(({400:'输入不符合要求，请检查字段。',401:'请重新登录。',403:'你没有执行此操作的权限。',409:'状态已改变，请刷新后重试。',429:'请求过于频繁，请稍后再试。',503:'身份源暂不可用。请核对 Tailnet ID、OAuth Client Secret、只读设备权限和主控的 API 网络，再重试。'})[r.status] || '请求失败，请检查资源状态后重试。'); e.status=r.status; throw e; }
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
function secretFields(f){const input=field(f,'client_secret','OAuth Client Secret · ','password');input.autocomplete='new-password';const link=el('a','OAuth 文档');link.href='https://tailscale.com/docs/features/oauth-clients';link.target='_blank';link.rel='noopener noreferrer';input.before(link,el('span',' · 只需 devices:core:read','muted'));return input;}
async function tailnets(){
  page('我的 Tailnet','使用稳定的 Tailnet ID 绑定身份源。OAuth 只请求设备只读权限，原始客户端权限由所有者配置。');
  const [items,retention]=await Promise.all([api('/tailnets'),api('/settings/retention')]);
  table(card('已绑定 Tailnet'),['名称 / ID','资源 / 凭据','完整身份成功 / 缓存截止','操作'],items.map(t=>[
    actions(el('span',t.display_name),el('code',t.api_id)),actions(status(t.enabled?'active':'paused'),status(t.credential_status)),
    actions(el('span',time(t.last_identity_success)),el('span',t.last_identity_success?'缓存至 '+time(t.last_identity_success+retention.identity_seconds):'尚无身份缓存')),
    actions(button('替换凭据',()=>credential(t)),button(t.enabled?'暂停':'启用',async()=>{await api('/tailnets/'+t.id+'/enabled','POST',{enabled:!t.enabled});await render();}),button('删除凭据',async()=>{if(!confirm('删除凭据会撤销设备许可，确认继续？'))return;await api('/tailnets/'+t.id+'/credential','DELETE');await render();},'danger'),button('删除 Tailnet',async()=>{if(!confirm('删除 Tailnet 会终止所有相关授权。已离线节点的旧许可受原缓存期限约束。确认继续？'))return;await api('/tailnets/'+t.id,'DELETE');await render();},'danger'),...(actor.role==='admin'?[button('转移',()=>transfer(t))]:[]))
  ]));
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
  page('我的服务器','注册后仍需接收并应用策略。暂停会发布空许可；删除和域名变更后，需要使用者更新自己的 DERP map。');
  const items=await api('/nodes');
	if(actor.role==='admin') {
	 const local=await api('/local/status');
	 if(!local.joined)$('content').append(button('注册本机中继',async()=>{await api('/local/register','POST',{display_name:local.saved.hostname||'本机中继'});await render();}));
	}
  table(card('服务器'),['名称','域名 / 公开端口','状态','最近心跳','操作'],items.map(n=>[
    n.display_name,el('code',n.domain+' · DERP TCP '+n.derp_port+' / STUN UDP '+n.stun_port),actions(status(n.state),...(!n.enabled?[status('paused')]:[])),time(n.last_heartbeat),
    actions(button('状态',()=>nodeState(n)),button('公开端口',()=>nodePorts(n)),...(n.state!=='pending'?[button('规则',()=>qos(n,true))]:[]),button(n.state==='pending'?'注册码':'重新签发注册码',()=>enrollment(n)),button(n.enabled?'暂停':'启用',async()=>{await api('/nodes/'+n.id+'/enabled','POST',{enabled:!n.enabled});await render();}),...(n.state!=='pending'?[button('域名',()=>changeDomain(n))]:[]),button('删除',()=>deleteNode(n),'danger'))
  ]));
  const f=form(card('添加服务器'),'创建并签发注册码',async data=>{const result=await api('/nodes','POST',{display_name:data.get('name'),domain:data.get('domain'),derp_port:Number(data.get('derp_port')),stun_port:Number(data.get('stun_port'))});return()=>showEnrollment(result.enrollment);});field(f,'name','显示名称');field(f,'domain','公共 DERP 域名');publicPortFields(f,{derp_port:443,stun_port:3478});done(f);
}
function publicPortFields(f,n) {
  for(const [name,label] of [['derp_port','公开 DERP TCP 端口'],['stun_port','公开 STUN UDP 端口']]){const input=field(f,name,label,'number',true,n[name]);input.min='1';input.max='65535';input.step='1';}
}
async function nodePorts(n) {
  const c=card(n.display_name+' · 公开端口');
  c.append(el('p','填写客户端访问的宿主机映射或代理端口。先配置对应入口，再保存并更新使用者的 DERP map；节点域名注册证明仍使用 HTTPS 443。','muted'));
  const f=form(c,'保存公开端口',data=>api('/nodes/'+n.id+'/ports','POST',{derp_port:Number(data.get('derp_port')),stun_port:Number(data.get('stun_port'))}));
  publicPortFields(f,n);done(f);c.scrollIntoView({behavior:'smooth'});
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
async function enrollment(n){if(n.state!=='pending'&&!confirm('重新签发会释放当前节点实例、关闭控制连接并撤销已有共享授权。重新加入后需要重新授权。确认继续？'))return;const e=await api('/nodes/'+n.id+'/enrollment','POST',{});await render();showEnrollment(e);}
function probeTable(parent,probes,node) {
  table(parent,['主控独立探测','结果','检查时间'],['derp','stun'].map(name=>[name==='derp'?'HTTPS DERP '+node.domain+':'+node.derp_port:'UDP STUN '+node.domain+':'+node.stun_port,probes?.[name]?.state==='ok'?'成功':probes?.[name]?.state==='failed'?'失败':'尚未检查',time(probes?.[name]?.observed_at)]));
  parent.append(el('p','探测仅表示主控当次能访问该端点。客户端是否能从所在网络连接，需要实际验证。','muted'));
}
function ruleSummary(parent,q) {
  const total=q.owner_weight+q.shared_weight;
  table(parent,['预算 / 组','争用时份额','硬上限'],[
    ['RX / TX 各 '+(q.budget_bps/1000000)+' Mbps','按实际字节计量','各方向受总预算限制'],
    ['owner 组',(q.owner_weight/total*100).toFixed(1)+'%（权重 '+q.owner_weight+'）','总预算'],
    ['shared 组',(q.shared_weight/total*100).toFixed(1)+'%（权重 '+q.shared_weight+'）',q.shared_max_bps?(q.shared_max_bps/1000000)+' Mbps':'未另设上限']
  ]);
  parent.append(el('p','份额用于双方均有需求时的分配；需求不足的一组会让出闲置容量，硬上限持续有效。组内 Tailnet 按权重分配，增加共享连接不会增加份额。','muted'));
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
  table(c,['Tailnet','有效设备数','完整身份刷新','身份缓存截止','主控联系截止','明确授权截止'],s.grants.map(g=>[g.tailnet_id,g.valid_keys,time(g.last_identity_success),time(g.identity_until),time(g.control_until),g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until)]));
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
    table(c,['Tailnet','累计 RX / TX 载荷（字节）','平均 RX / TX（Mbps）','采样间隔','排队载荷（字节）'],s.report.traffic.map(t=>{const r=rates.get(t.tailnet_id);return[t.tailnet_id,t.rx_payload_bytes+' / '+t.tx_payload_bytes,r?(r.rx_bits_per_second/1000000).toFixed(3)+' / '+(r.tx_bits_per_second/1000000).toFixed(3):'等待两次连续计数样本',r?r.interval_seconds.toFixed(1)+' 秒':'—',t.queued_payload_bytes];}));
    c.append(el('p','计数来自节点 derper；累计值从当前进程的相应 Tailnet 计数开始。平均值只覆盖最近两次采样之间的 DERP 载荷，不包含协议开销，也不证明对端应用收到数据。','muted'));
  } else c.append(el('p','尚无当前节点实例的运行报告。','muted'));
  c.append(el('h2','主控独立端点探测'));probeTable(c,s.probes,s.node);
  if((actor.role==='admin'||n.owner_id===actor.id)&&['registered','ready','offline'].includes(s.node.state))c.append(button('立即检查端点',async()=>{await api('/nodes/'+n.id+'/probe','POST',{});c.remove();await nodeState(n);}));
  c.scrollIntoView({behavior:'smooth'});
}
async function directory(){
  page('服务器目录','选择你的 Tailnet 申请共享。提供者批准后，需要申请方确认，授权才会生效。');
  const [nodes,tailnets]=await Promise.all([api('/relays'),api('/tailnets')]);
  const summaries=await Promise.all(nodes.map(n=>api('/nodes/'+n.id+'/summary')));
  table(card('可申请服务器'),['服务器 / 提供者','域名 / 公开端口','最近心跳','操作'],summaries.map(s=>[actions(el('span',s.node.display_name),el('span','提供者 '+s.provider)),el('code',s.node.domain+' · DERP TCP '+s.node.derp_port+' / STUN UDP '+s.node.stun_port),actions(status(s.node.state),el('span',time(s.node.last_heartbeat))),actions(button('申请',()=>request(s.node,tailnets)),button('规则与探测',()=>{const c=card(s.node.display_name+' · 共享信息');ruleSummary(c,s.qos);c.append(el('p','域名注册验证：'+time(s.domain_verified_at)));probeTable(c,s.probes,s.node);c.scrollIntoView({behavior:'smooth'});}))]));
}
async function request(n,tailnets){if(!tailnets.length)throw new Error('请先绑定自己的 Tailnet。');const c=card('申请 '+n.display_name);const f=form(c,'提交申请',async data=>{const existing=await api('/grants');const g=existing.find(g=>g.node_id===n.id&&g.tailnet_id===data.get('tailnet'));await api('/grants','POST',{node_id:n.id,tailnet_id:data.get('tailnet'),expected_revision:g?.revision||0,explicit_until:data.get('until')?new Date(data.get('until')).toISOString():'0001-01-01T00:00:00Z'});});select(f,'tailnet','使用 Tailnet',tailnets.filter(t=>actor.role==='admin'||t.owner_id===actor.id).map(t=>[t.id,t.display_name]));field(f,'until','明确截止（可选）','datetime-local',false);done(f);c.scrollIntoView({behavior:'smooth'});}
async function grants(){page('申请与授权','申请 → 提供者批准 → 申请方确认。撤销后重新申请会产生新版本，凭据恢复不会重新激活旧授权。');const items=await api('/grants');table(card('授权关系'),['服务器','Tailnet','状态','截止 / 版本','操作'],items.map(g=>{const owner=actor.role==='admin'||g.node_owner_id===actor.id,applicant=actor.role==='admin'||g.tailnet_owner_id===actor.id;const available=[];if(g.state==='requested'){if(owner)available.push(['approve','批准'],['reject','拒绝']);if(applicant)available.push(['cancel','取消']);}if(g.state==='owner_approved'){if(applicant)available.push(['confirm','确认'],['cancel','取消']);if(owner)available.push(['revoke','撤销']);}if(g.state==='active'){if(owner)available.push(['revoke','撤销']);if(applicant)available.push(['leave','退出']);}return[g.node_name,actions(el('span',g.tailnet_name),el('span','申请者 '+g.applicant)),status(g.state),(g.explicit_until==='0001-01-01T00:00:00Z'?'未另设截止':time(g.explicit_until))+' · r'+g.revision,actions(...available.map(([action,label])=>button(label,async()=>{if(['revoke','leave'].includes(action)&&!confirm('确认'+label+'此授权？'))return;await api('/grants/'+g.id+'/actions','POST',{expected_revision:g.revision,action});await render();})),...(['requested','owner_approved','active'].includes(g.state)?[button('当前规则',()=>qos({id:g.node_id,display_name:g.node_name},false)),button('观测状态',()=>nodeState({id:g.node_id,display_name:g.node_name,owner_id:g.node_owner_id}))]:[]))];}));}
async function qos(n,editable){const q=await api('/nodes/'+n.id+'/qos');const c=card(n.display_name+' · 带宽规则');c.append(el('p','争用时按字节分配组份额；需求不足时可借用闲置份额，硬上限仍有效。RX 和 TX 各使用同一预算。','muted'));ruleSummary(c,q);if(!editable){table(c,['你的 Tailnet','分组','组内权重','硬上限'],q.tailnets.map(r=>[r.tailnet_id,r.group,r.weight,r.max_bps?(r.max_bps/1000000)+' Mbps':'未另设上限']));return;}const tailnets=await api('/tailnets');const own=new Set(tailnets.filter(t=>t.owner_id===n.owner_id).map(t=>t.id));const f=form(c,'热应用规则',async(data)=>{const body={budget_bps:Math.round(Number(data.get('budget'))*1000000),owner_weight:Number(data.get('owner')),shared_weight:Number(data.get('shared')),shared_max_bps:Math.round(Number(data.get('shared_max')||0)*1000000),tailnets:q.tailnets.map((r,i)=>({tailnet_id:r.tailnet_id,group:data.get('group'+i),weight:Number(data.get('weight'+i)),max_bps:Math.round(Number(data.get('max'+i)||0)*1000000)}))};await api('/nodes/'+n.id+'/qos','POST',body);});for(const [name,label,value]of[['budget','总预算（Mbps）',q.budget_bps/1000000],['owner','owner 权重',q.owner_weight],['shared','shared 权重',q.shared_weight],['shared_max','shared 硬上限（Mbps，0 表示不单设上限）',q.shared_max_bps/1000000]]){const input=field(f,name,label,'number',true,value);input.min=['owner','shared'].includes(name)?'1':name==='shared_max'?'0':'0.000008';input.step=['owner','shared'].includes(name)?'1':'any';}q.tailnets.forEach((r,i)=>{f.append(el('h2',r.tailnet_id));const group=select(f,'group'+i,'分组',own.has(r.tailnet_id)?[['owner','owner'],['shared','shared']]:[['shared','shared']]);group.value=r.group;field(f,'weight'+i,'Tailnet 权重','number',true,r.weight);field(f,'max'+i,'Tailnet 硬上限（Mbps，0 表示不单设上限）','number',true,r.max_bps/1000000);});done(f);c.scrollIntoView({behavior:'smooth'});}
async function map(){page('DERP map','生成标准 DERPMap 片段，并自行合并到 Tailnet 策略。保留已有 Regions 和默认 region；这项操作不会写入 Tailnet 配置。');const items=await api('/tailnets');const c=card('选择 Tailnet');select(c,'tailnet','Tailnet',items.map(t=>[t.id,t.display_name]));c.append(button('生成片段',async()=>{const result=await api('/tailnets/'+c.querySelector('select').value+'/derpmap');let output=c.querySelector('textarea');if(!output){output=el('textarea');output.readOnly=true;output.setAttribute('aria-label','DERPMap JSON');c.append(output);}output.value=JSON.stringify(result,null,2);}));}
async function account(){page('我的账号','密码修改后已有会话会失效，请重新登录。');const f=form(card('修改密码'),'更新密码',async(data,f)=>{const password=data.get('password');f.elements.password.value='';await api('/users/'+actor.id+'/password','POST',{password});await session();});const input=field(f,'password','新密码（12–72 字节）','password');input.autocomplete='new-password';done(f);}
async function users(){page('账号管理','管理员代操作始终记录当前管理员身份。');const items=await api('/users');table(card('集群账号'),['用户名','角色','状态','操作'],items.map(u=>[u.username,u.role,u.enabled?'启用':'暂停',actions(button(u.enabled?'暂停':'启用',async()=>{await api('/users/'+u.id+'/enabled','POST',{enabled:!u.enabled});await render();}),button('重置密码',()=>password(u)))]));const f=form(card('创建成员'),'创建账号',async(data,f)=>{const body={username:data.get('username'),password:data.get('password')};f.elements.password.value='';await api('/users','POST',body);});field(f,'username','用户名');field(f,'password','初始密码（12–72 字节）','password');done(f);}
async function password(u){const f=form(card('重置 '+u.username+' 的密码'),'重置密码',async(data,f)=>{const password=data.get('password');f.elements.password.value='';await api('/users/'+u.id+'/password','POST',{password});});field(f,'password','新密码','password');done(f);f.scrollIntoView({behavior:'smooth'});}
async function settings(){page('集群设置','身份源和主控联系各有独立的离线保留期。失败或重发快照不会更新最后成功时间。');const q=await api('/settings/retention');const f=form(card('离线保留'),'保存保留期',data=>api('/settings/retention','POST',{identity_seconds:Number(data.get('identity')),control_seconds:Number(data.get('control'))}));field(f,'identity','身份缓存保留（秒）','number',true,q.identity_seconds);field(f,'control','主控联系保留（秒）','number',true,q.control_seconds);done(f);}
async function events(){page('事件与审计','各展示最近 200 条可见记录；已恢复的连续故障保留恢复时间。');const [events,audit]=await Promise.all([api('/events'),api('/audit')]);table(card('站内事件'),['时间','资源','事件','状态'],events.map(e=>[time(e.created_at),e.resource_id,e.message,e.resolved_at?'结束 / 恢复于 '+time(e.resolved_at):'待处理']));table(card('操作审计'),['时间','实际操作者','动作','资源'],audit.map(a=>[time(a.created_at),a.actor_username||a.actor_id,a.action,a.resource_id]));}
function localState(s) {
 const c=card('本机运行状态');
 table(c,['项目','状态'],[['角色',s.role==='setup'?'等待初始设置':s.role==='controller'?'主控':'成员节点'],['集群连接',s.joined?(s.control.connected?'已连接主控':'已注册，主控连接中断'):'尚未加入'],['中继许可',s.control.usable?'当前有有效许可':'当前没有有效许可'],['已接收 / 已应用策略',s.control.received_revision+' / '+s.control.applied_revision],['主控下发总预算',s.policy_budget_bps?s.policy_budget_bps/1000000+' Mbps':'尚无策略'],['本地总限速',s.active.max_budget_bps?s.active.max_budget_bps/1000000+' Mbps':'不另设上限'],['当前有效调度预算',s.effective_budget_bps?s.effective_budget_bps/1000000+' Mbps':'尚未应用'],['配置应用',s.pending_apply?'已保存，待应用':'已应用']]);
 if(s.apply_error)c.append(el('p','最近应用失败：'+s.apply_error,'error'));
 const fields=[['role','角色'],['hostname','公共 DERP 域名'],['derp_listen','DERP TCP 监听'],['stun_listen','STUN UDP 监听'],['tls_mode','TLS 模式'],['cert_mode','证书方式'],['derp_port','公开 DERP TCP 端口'],['stun_port','公开 STUN UDP 端口'],['max_budget_bps','本地总限速（bps）'],['logging_level','日志级别']];
 const differences=fields.filter(([key])=>s.saved[key]!==s.active[key]);
 if(differences.length)table(c,['待应用项目','运行中配置','已保存配置'],differences.map(([key,label])=>[label,String(s.active[key]),String(s.saved[key])]));
 table(c,['最后联系主控','流量采样时间','活动 DERP 连接'],[[time(s.control.last_contact),time(s.control.traffic_observed_at),s.control.active_connections]]);
 if(s.control.traffic?.length)table(c,['Tailnet','累计 RX / TX 载荷（字节）','排队载荷（字节）'],s.control.traffic.map(t=>[t.tailnet_id,t.rx_payload_bytes+' / '+t.tx_payload_bytes,t.queued_payload_bytes]));
 return c;
}

function localConfigForm(s,role,initial=false) {
 const c=card(role==='controller'?'主控本机配置':'成员节点本机配置');
 const q=s.saved;
 c.append(el('p','公开端口填写客户端访问的入口；监听地址填写容器或本机的实际地址。端口映射、DNS 和 HTTPS 反代需在宿主机配置。','muted'));
 const read=data=>({role,hostname:data.get('hostname'),derp_listen:data.get('derp_listen'),stun_listen:data.get('stun_listen'),tls_mode:data.get('tls_mode'),cert_mode:data.get('cert_mode'),derp_port:Number(data.get('derp_port')),stun_port:Number(data.get('stun_port')),max_budget_bps:Math.round(Number(data.get('limit'))*1000000),logging_level:data.get('logging_level')});
 const f=form(c,initial?'保存并应用初始设置':'保存配置',async data=>{await api('/local/settings','POST',read(data));if(initial){try{await api('/local/apply','POST',{});await session();}catch(e){await render();throw e;}}});
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
 if(!initial)c.append(button('应用已保存配置',async()=>{try{await api('/local/apply','POST',{});await session();}catch(e){await render();throw e;}}));
 const certCard=card('上传手动证书');
 certCard.append(el('p','上传与已保存域名匹配的 PEM 证书链和私钥，上传后应用配置。独立管理入口的 HTTPS 由宿主机反代提供。','muted'));
 const upload=form(certCard,'保存证书',async(data,f)=>{const certificate=f.elements.certificate.files[0],key=f.elements.private_key.files[0];if(!certificate||!key)throw new Error('请选择证书链和私钥文件。');await api('/local/certificate','POST',{certificate:await certificate.text(),private_key:await key.text()});f.reset();});
 field(upload,'certificate','PEM 证书链','file');field(upload,'private_key','PEM 私钥','file');done(upload);
}

async function bootstrap(){
 const s=await api('/local/status');
 page('初始设置','选择本机角色。每台服务器使用自己的管理员账号，成员节点与主控失联时仍可登录本机管理面板。');
 localState(s);
 const chooser=card('选择角色');
 chooser.append(actions(button('配置为主控',()=>{document.querySelectorAll('#content .card').forEach(c=>{if(c!==chooser)c.remove();});localConfigForm(s,'controller',true);}),button('加入已有集群',()=>{document.querySelectorAll('#content .card').forEach(c=>{if(c!==chooser)c.remove();});localConfigForm(s,'member',true);})));
 if(s.saved.role!=='setup')localConfigForm(s,s.saved.role,true);
}

async function localSettings(){const s=await api('/local/status');page('本机设置','保存配置后单独应用。应用会重启本机中继，管理面板保持可访问。');localState(s);localConfigForm(s,s.role);}

async function member(){
 const s=await api('/local/status');page('集群连接','本机管理员管理加入和退出。共享授权、分组权重及 Tailnet 优先级由资源所有者在主控面板配置。');
 const c=localState(s);
 if(s.qos){const rules=card('主控下发规则');ruleSummary(rules,s.qos);table(rules,['Tailnet','分组','权重','硬上限'],s.qos.tailnets.map(t=>[t.tailnet_id,t.group,t.weight,t.max_bps?t.max_bps/1000000+' Mbps':'未另设上限']));}
 if(s.joined){table(c,['主控地址','集群 ID','节点 ID'],[[s.controller_url,s.cluster_id,s.node_id]]);c.append(button('退出集群',async()=>{if(!confirm('退出会立即关闭本机 DERP 连接并清除许可。重新加入需要主控签发新注册码及重新授权。确认继续？'))return;const result=await api('/local/leave','POST',{});await render();notice(result.released?'本机已退出，主控已释放节点。':'本机已退出；主控暂未确认释放，请在主控重新签发注册码。');},'danger'));}
 else {const f=form(card('加入已有集群'),'验证并加入',async(data,f)=>{const body={controller_url:data.get('controller_url'),enrollment_code:data.get('enrollment_code')};f.elements.enrollment_code.value='';await api('/local/join','POST',body);});field(f,'controller_url','主控 HTTPS 地址','url',true,s.saved.controller_url||s.controller_url);const code=field(f,'enrollment_code','一次性注册码','password');code.autocomplete='off';done(f);}
}

const views={tailnets,nodes,directory,grants,map,account,users,settings,events,bootstrap,local:localSettings,member};
async function render(){if(!actor)return;let name=location.hash.slice(1);const allowed=serverRole==='setup'?['bootstrap','account']:serverRole==='member'?['member','local','account']:Object.keys(views).filter(n=>n!=='bootstrap'&&n!=='member');if(actor.role!=='admin')allowed.splice(allowed.indexOf('local'),allowed.includes('local')?1:0);if(!allowed.includes(name))name=allowed[0];document.querySelectorAll('nav a').forEach(a=>{a.hidden=!allowed.includes(a.hash.slice(1))||(a.hasAttribute('data-admin')&&actor.role!=='admin');a.setAttribute('aria-current',a.hash==='#'+name?'page':'false');});try{await (views[name]||tailnets)();}catch(e){notice(e.message,true);if(e.status===401)await session();}}
async function session(){try{const result=await api('/session');actor=result.actor;csrf=result.csrf_token;serverRole=result.server_role;}catch(e){actor=csrf=serverRole=undefined;}const logged=!!actor;let needsSetup=false;if(!logged){try{needsSetup=(await api('/setup')).required;}catch(e){notice(e.message,true);}}$('setup').hidden=logged||!needsSetup;$('login').hidden=logged||needsSetup;$('workspace').hidden=!logged;$('account').hidden=!logged;if(!logged)$('content').replaceChildren();if(logged){$('account-name').textContent=actor.username;await render();}}
$('setup-form').addEventListener('submit',async e=>{e.preventDefault();const f=e.currentTarget;const data=new FormData(f);const body={username:data.get('username'),password:data.get('password')};if(body.password!==data.get('password_confirm')){notice('两次输入的密码不一致',true);return;}f.elements.password.value=f.elements.password_confirm.value='';const b=f.querySelector('button');b.disabled=true;try{await api('/setup','POST',body);await session();}catch(err){notice(err.message,true);if(err.status===409)await session();}finally{b.disabled=false;}});
$('login-form').addEventListener('submit',async e=>{e.preventDefault();const f=e.currentTarget;const data=new FormData(f);const body={username:data.get('username'),password:data.get('password')};f.elements.password.value='';const b=f.querySelector('button');b.disabled=true;try{await api('/login','POST',body);await session();}catch(err){notice(err.message,true);}finally{b.disabled=false;}});
$('logout').addEventListener('click',async()=>{try{await api('/logout','POST',{});actor=csrf=undefined;$('content').replaceChildren();await session();}catch(e){notice(e.message,true);}});
window.addEventListener('hashchange',render);
session();
