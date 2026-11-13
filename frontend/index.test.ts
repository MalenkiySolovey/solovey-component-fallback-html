import { beforeEach, describe, expect, it, vi } from 'vitest'
const owner=vi.hoisted(()=>({register:vi.fn(),get:vi.fn(),post:vi.fn(),mounted:vi.fn()}))
vi.mock('@solovey/component-ui',()=>({registerComponent:owner.register}))
vi.mock('@solovey/component-ui/api',()=>({default:{get:owner.get,post:owner.post}}))
vi.mock('vue-i18n',()=>({useI18n:()=>({t:(key:string)=>key})}))
vi.mock('vue',async original=>({...await original<typeof import('vue')>(),onMounted:owner.mounted}))
import { register } from './index'
import { usePublicSite } from './usePublicSite'
import en from './locales/en'
import ru from './locales/ru'
const keys=(v:Record<string,unknown>,prefix=''):string[]=>Object.entries(v).flatMap(([k,x])=>typeof x==='object'&&x!==null?keys(x as Record<string,unknown>,prefix+k+'.'):[prefix+k]).sort()
beforeEach(()=>vi.clearAllMocks())
describe('Fallback UI contribution',()=>{
 it('preserves route, navigation, Classic and Nexus icons and locale loaders',()=>{register();expect(owner.register).toHaveBeenCalledTimes(1);const c=owner.register.mock.calls[0]![0];expect(c.id).toBe('fallback-html');expect(c.apiVersion).toBe('1.0');expect(c.routes).toEqual([expect.objectContaining({path:'/fallback-html',name:'component.fallback-html',component:expect.any(Function)})]);expect(c.nav).toEqual([expect.objectContaining({path:'/fallback-html',order:122,section:'integrations',icon:'mdi-home',nexusIcon:'lucide:globe'})]);expect(Object.keys(c.locales).sort()).toEqual(['en','ru'])})
 it('retains complete English and Russian form and workflow translations',()=>{expect(keys(ru)).toEqual(keys(en));expect(keys(en)).toContain('fallbackHtml.title')})
 it('uses the provided authenticated client and preserves denied and network errors',async()=>{const page=usePublicSite();owner.get.mockResolvedValueOnce({data:{success:false,msg:'denied',obj:[]}});await page.loadSites();expect(owner.get).toHaveBeenCalledWith('api/components/fallback-html/sites');expect(page.error.value).toBe('denied');expect(page.loading.value).toBe(false);owner.get.mockRejectedValueOnce(Error('offline'));await page.loadSites();expect(page.error.value).toBe('offline');expect(page.loading.value).toBe(false)})
 it('retains disabled and published status and template editing rules',()=>{const page=usePublicSite();const site={id:7,name:'Portal',templateId:'generated-portal',redirects:[],enabled:false,status:'published',pages:[]} as Parameters<typeof page.siteStatusText>[0];expect(page.siteStatusText(site)).toBe('fallbackHtml.statusDisabled');expect(page.siteStatusColor(site)).toBe('warning');site.enabled=true;expect(page.siteStatusText(site)).toBe('fallbackHtml.statusPublished');expect(page.siteStatusColor(site)).toBe('success')})
})
