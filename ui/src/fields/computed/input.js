// {
//     collection: undefined,
//     originalRecord: undefined,
//     record: undefined,
//     field: undefined,
// }
export function input(props) {
    // computed fields are read-only; no edit input is rendered
    return t.div(
        { className: "record-field-input field-type-computed" },
        t.div(
            { className: "field" },
            t.label({}, () => props.field.name),
            t.div({ className: "field-help" }, "Read-only computed field"),
        ),
    );
}
