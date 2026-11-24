const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");

const script = fs.readFileSync(path.join(__dirname,"component.js"),"utf8");
async function component() {
  const state = {Card:{type:"card",brands:["visa"]}, Version:4, CSRF:"test-csrf", ClientKey:"test_demo", Amount:{Minor:2599,Currency:"USD"}, ExpiresAt:new Date(Date.now()+60000).toISOString()};
  const posts = [], replies = [], mounts = [];
  let config;
  const window = {parent:{postMessage(){}},top:{},self:{}};
  window.AdyenWeb = {AdyenCheckout: async c => {config = c; return {};}, Card:class {mount(){mounts.push("card");}}};
  const node = {replaceChildren(){}};
  await vm.runInNewContext(script,{
    window,document:{getElementById:()=>node},URL,location:new URL("https://checkout.example.test/orders/order/component"),
    setTimeout:()=>1,clearTimeout(){},
    fetch: async (url,opt) => {
      if (url.pathname.endsWith("/card-state")) return {ok:true,json:async()=>state};
      posts.push(JSON.parse(opt.body));
      return {ok:true,json:async()=>({resultCode:"Authorised"})};
    },
  });
  return {config,posts,mounts,actions:{reject:()=>replies.push("rejected"),resolve:()=>replies.push("resolved")},replies};
}
function hostedInput(){
  const jwe = "eyJfixture..a.b.c";
  return {isValid:true,data:{paymentMethod:{type:"scheme",brand:"visa",holderName:"Test",encryptedCardNumber:jwe,encryptedExpiryYear:jwe,encryptedExpiryMonth:jwe,encryptedSecurityCode:jwe},browserInfo:{userAgent:"fixture"},irrelevant:"never forward"}};
}
test("projects the encrypted method and permitted browser fields",async()=>{
  const ui=await component(),input=hostedInput();
  input.data.paymentMethod.number="excluded";
  input.data.browserInfo.extra="excluded";
  await ui.config.onSubmit(input,null,ui.actions);
  assert.deepEqual(ui.mounts,["card"]);
  assert.equal(ui.posts.length,1);
  assert.equal(ui.posts[0].paymentMethod.number,undefined);
  assert.deepEqual(ui.posts[0].browserInfo,{userAgent:"fixture"});
  assert.deepEqual(ui.replies,["resolved"]);
});
test("rejects non-JWE card fields before making a host request",async()=>{
  const ui=await component(),input=hostedInput();
  input.data.paymentMethod.encryptedSecurityCode="unwrapped";
  await ui.config.onSubmit(input,null,ui.actions);
  assert.deepEqual(ui.posts,[]);
  assert.deepEqual(ui.replies,["rejected"]);
});
