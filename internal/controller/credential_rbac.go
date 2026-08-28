package controller

// CredentialRequestReconciler RBAC declarations are kept in a package-level
// file so controller-gen includes them alongside the existing controllers.
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=credentialrequests,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=credentialrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=credentialapprovals,verbs=get;list;watch
// +kubebuilder:rbac:groups=typeclaw.fml09.io,resources=typeclawinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
