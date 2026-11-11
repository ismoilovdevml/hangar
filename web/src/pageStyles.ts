// Page style sheets, in the order they had when every page was bundled into
// one script. Pages load lazily (router.tsx), but their styles stay in the
// one style sheet loaded upfront, in this order and before the global styles
// (styles/index.css), so rules of equal specificity resolve as before and no
// page restyles another once it has been opened.
import './pages/access/access.css';
import './pages/changePassword.css';
import './pages/apidocs/apidocs.css';
import './pages/repositories/repoForm.css';
import './styles/catalog.css';
import './pages/repositories/settings/settings.css';
import './pages/browse/browse.css';
import './pages/search/search.css';
import './pages/blobstores/blobstores.css';
import './pages/cluster/cluster.css';
import './pages/vulnerabilities/vulnerabilities.css';
import './pages/settings/hub.css';
import './pages/settings/settings.css';
import './pages/system/system.css';
