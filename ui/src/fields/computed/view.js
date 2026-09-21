// {
//     record: undefined,
//     field: undefined,
//     short: false,
// }
export function view(props) {
    const value = props.record ? props.record[props.field.name] : undefined;

    let display;
    if (value === null || value === undefined) {
        display = t.span({ className: "txt-muted" }, "—");
    } else if (typeof value === "object") {
        display = t.code({ className: "field-type-computed-json" }, JSON.stringify(value));
    } else {
        display = String(value);
    }

    return t.div(
        { className: "record-field-view field-type-computed" },
        t.span({ className: "label" }, display),
    );
}
