import{$a as e,Co as t}from"./index-B4yp8Cbr.js";var n=t(),r=`
.demo-note {
  position: fixed;
  right: 12px;
  bottom: 12px;
  z-index: 60;
  display: flex;
  gap: 8px;
  align-items: center;
  margin: 0;
  padding: 5px 11px;
  border: 1px solid var(--hairline-strong);
  border-radius: 999px;
  background: var(--glass-bg-strong);
  box-shadow: var(--shadow-raised);
  color: var(--ink-3);
  font-size: 12px;
  line-height: 1.4;
  pointer-events: none;
  backdrop-filter: var(--glass-blur);
}
.demo-note b {
  color: var(--lake-ink);
  font-weight: 700;
}
`;function i(){let t=e({select:e=>e.location.pathname===`/login`});return(0,n.jsxs)(n.Fragment,{children:[(0,n.jsx)(`style`,{children:r}),(0,n.jsxs)(`p`,{className:`demo-note`,role:`note`,children:[(0,n.jsx)(`b`,{children:`Demo`}),t?`Sign in with demo / demo`:`Sample data, changes reset on reload`]})]})}export{i as default};