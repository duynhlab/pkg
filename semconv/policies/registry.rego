package before_resolution

import rego.v1

# ADR-076 namespace rule. A dotted attribute this registry DEFINES must start
# with a registered platform namespace. Adding one is a registry change that
# names its owner here; nothing is added at a call site. Single-segment keys
# are not namespaced (owner decision 2026-09-24) — they only have to be defined.
platform_namespaces := {
	# the domain services
	"cart", "checkout", "inventory", "notification", "order", "payment",
	"product", "review", "shipping", "user",
	# owned by a service, registered as found in the 2026-09-24 inventory
	"availability", "cache", "downstream", "estimate", "inventory_service", # product, shipping
	"item", "items", "request",                                             # cart, checkout
	"notifications", "email", "sms", "result",                               # notification
	"orders", "processing", "compensation",                                  # order
	"refund", "reconciliation", "discrepancy", "webhook",                    # payment
	"products", "profile", "review_service", "reviews",                      # product, user, review
	"quote", "shipment", "tracking", "order_id",                             # shipping
	"session",                                                               # checkout
	# shared package and platform
	"temporal", "platform", "api", "pgx", "pyroscope",
	# review-service names its gRPC truncation counter under grpc.*
	"grpc",
}

# Namespaces the pinned upstream conventions own: reused by ref, never redefined.
upstream_namespaces := {
	"http", "rpc", "db", "messaging", "error", "exception", "service",
	"deployment", "k8s", "code", "url", "server", "client", "network",
	"user_agent", "telemetry", "otel", "process", "host", "container",
}

first_segment(name) := split(name, ".")[0]

vendor(g) if g.annotations.origin == "vendor"

deny contains finding if {
	some g in input.groups
	some attr in g.attributes
	name := attr.id
	contains(name, ".")
	upstream_namespaces[first_segment(name)]
	finding := {"id": "upstream_redefined", "type": "semconv_attribute", "category": "namespace", "group": g.id, "attr": name}
}

deny contains finding if {
	some g in input.groups
	some attr in g.attributes
	name := attr.id
	contains(name, ".")
	ns := first_segment(name)
	not platform_namespaces[ns]
	not upstream_namespaces[ns]
	finding := {"id": "unregistered_namespace", "type": "semconv_attribute", "category": "namespace", "group": g.id, "attr": name}
}

# Every metric has a UCUM unit (ADR-073/076). A library's instrument keeps the
# unit its library chose, which is why vendor groups are exempt.
deny contains finding if {
	some g in input.groups
	g.type == "metric"
	not vendor(g)
	object.get(g, "unit", "") == ""
	finding := {"id": "metric_without_unit", "type": "semconv_attribute", "category": "metric", "group": g.id, "attr": g.id}
}

deny contains finding if {
	some g in input.groups
	g.type != "attribute_group"
	not g.stability
	finding := {"id": "group_without_stability", "type": "semconv_attribute", "category": "stability", "group": g.id, "attr": g.id}
}

deny contains finding if {
	some g in input.groups
	some attr in g.attributes
	attr.id
	not attr.stability
	finding := {"id": "attribute_without_stability", "type": "semconv_attribute", "category": "stability", "group": g.id, "attr": attr.id}
}

# Metric and event names live in namespaces too. A business instrument must
# start with a registered namespace; a library's instrument (origin: vendor)
# keeps its own name. The two process.* lifecycle events are the one deliberate
# use of an upstream namespace: the catalog froze them (ADR-071) and they name
# the process, which is what that namespace is for.
deny contains finding if {
	some g in input.groups
	g.type == "metric"
	not vendor(g)
	ns := first_segment(g.metric_name)
	not platform_namespaces[ns]
	finding := {"id": "unregistered_metric_namespace", "type": "semconv_attribute", "category": "namespace", "group": g.id, "attr": g.metric_name}
}

deny contains finding if {
	some g in input.groups
	g.type == "event"
	ns := first_segment(g.name)
	not platform_namespaces[ns]
	ns != "process"
	finding := {"id": "unregistered_event_namespace", "type": "semconv_attribute", "category": "namespace", "group": g.id, "attr": g.name}
}
