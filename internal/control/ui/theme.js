'use strict';
const systemTheme=matchMedia('(prefers-color-scheme: dark)');
let themePreference='system';
try{const saved=localStorage.getItem('uniderp-theme');if(['system','light','dark'].includes(saved))themePreference=saved;}catch{}
function applyTheme(){
 document.documentElement.dataset.theme=themePreference==='system'?(systemTheme.matches?'dark':'light'):themePreference;
 document.querySelectorAll('[data-theme-choice]').forEach(choice=>choice.setAttribute('aria-checked',String(choice.dataset.themeChoice===themePreference)));
}
function setTheme(preference){
 themePreference=preference;
 try{localStorage.setItem('uniderp-theme',preference);}catch{}
 applyTheme();
}
systemTheme.addEventListener('change',()=>{if(themePreference==='system')applyTheme();});
applyTheme();
