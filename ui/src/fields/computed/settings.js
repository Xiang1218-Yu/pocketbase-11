// {
//     collection: undefined,
//     originalCollection: undefined,
//     field: undefined,
//     get fieldIndex: int/-1,
//     get originalField: undefined
// }
export function settings(props) {
    const uniqueId = "f_" + app.utils.randomString();

    const ensureDefaults = () => {
        props.field.mode = props.field.mode || "expression";
        props.field.resultType = props.field.resultType || "text";
        props.field.dependsOn = props.field.dependsOn || [];
        props.field.timeoutMs = props.field.timeoutMs || 100;
    };
    ensureDefaults();

    const otherFieldNames = (props.collection?.fields || [])
        .filter((f) => f.name !== props.field.name && f.type !== "computed")
        .map((f) => f.name);

    return app.components.fieldSettings(props, {
        content: () =>
            t.div(
                { className: "grid" },
                t.div(
                    { className: "col-12" },
                    t.div({ className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".mode" },
                            t.span({ className: "txt" }, "Evaluation mode"),
                        ),
                        t.select(
                            {
                                id: uniqueId + ".mode",
                                name: () => `fields.${props.fieldIndex}.mode`,
                                onchange: (e) => (props.field.mode = e.target.value),
                            },
                            t.option({ value: "expression", selected: () => props.field.mode === "expression" }, "Expression (safe, single expression)"),
                            t.option({ value: "function", selected: () => props.field.mode === "function" }, "Restricted function body"),
                        ),
                    ),
                ),
                t.div(
                    { className: "col-12" },
                    t.div({ className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".expression" },
                            t.span({ className: "txt required" }, "JavaScript expression"),
                            t.i({
                                className: "ri-information-line link-hint",
                                ariaDescription: app.attrs.tooltip(
                                    "Expression mode: evaluated as (doc, ctx) => (<expression>). Function mode: the body of function(doc, ctx) { ... }. No IO, database or network access is available.",
                                ),
                            }),
                        ),
                        t.textarea({
                            id: uniqueId + ".expression",
                            className: "monospace",
                            rows: 5,
                            name: () => `fields.${props.fieldIndex}.expression`,
                            placeholder: () =>
                                props.field.mode === "function"
                                    ? "return doc.title + ' #' + doc.id;"
                                    : "doc.title + ' #' + doc.id",
                            value: () => props.field.expression || "",
                            oninput: (e) => (props.field.expression = e.target.value),
                        }),
                    ),
                ),
                t.div(
                    { className: "col-sm-6" },
                    t.div({ className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".resultType" },
                            t.span({ className: "txt" }, "Result type"),
                        ),
                        t.select(
                            {
                                id: uniqueId + ".resultType",
                                name: () => `fields.${props.fieldIndex}.resultType`,
                                onchange: (e) => (props.field.resultType = e.target.value),
                            },
                            t.option({ value: "text", selected: () => props.field.resultType === "text" }, "Text"),
                            t.option({ value: "number", selected: () => props.field.resultType === "number" }, "Number"),
                            t.option({ value: "bool", selected: () => props.field.resultType === "bool" }, "Bool"),
                            t.option({ value: "json", selected: () => props.field.resultType === "json" }, "JSON"),
                        ),
                    ),
                ),
                t.div(
                    { className: "col-sm-6" },
                    t.div({ className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".timeoutMs" },
                            t.span({ className: "txt" }, "Timeout (ms)"),
                        ),
                        t.input({
                            type: "number",
                            id: uniqueId + ".timeoutMs",
                            name: () => `fields.${props.fieldIndex}.timeoutMs`,
                            min: 1,
                            max: 1000,
                            value: () => props.field.timeoutMs || 100,
                            oninput: (e) => (props.field.timeoutMs = parseInt(e.target.value, 10) || 100),
                        }),
                    ),
                ),
                t.div(
                    { className: "col-12" },
                    t.div({ className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".dependsOn" },
                            t.span({ className: "txt" }, "Depends on"),
                            t.i({
                                className: "ri-information-line link-hint",
                                ariaDescription: app.attrs.tooltip(
                                    "Comma-separated field names of the same collection the expression relies on. Hidden dependency fields cause the computed value to be hidden as well. Relation fields declared here are accessible through doc.$rel('fieldName') in function mode.",
                                ),
                            }),
                        ),
                        t.input({
                            type: "text",
                            id: uniqueId + ".dependsOn",
                            placeholder: "title,status,customer",
                            value: () => (props.field.dependsOn || []).join(","),
                            onchange: (e) => {
                                props.field.dependsOn = e.target.value
                                    .split(",")
                                    .map((s) => s.trim())
                                    .filter((s) => s !== "");
                            },
                        }),
                        t.div(
                            { className: "field-help" },
                            "Available fields: " + (otherFieldNames.join(", ") || "none"),
                        ),
                    ),
                ),
            ),
    });
}
