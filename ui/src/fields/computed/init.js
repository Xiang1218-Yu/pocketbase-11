import { input } from "./input";
import { settings } from "./settings";
import { view } from "./view";

window.app = window.app || {};
window.app.fieldTypes = window.app.fieldTypes || {};
window.app.fieldTypes.computed = {
    icon: "ri-function-line",
    label: "Computed (JS)",
    settings,
    input,
    view,
    // computed fields are read-only and never submitted
    dummyData: () => null,
};
