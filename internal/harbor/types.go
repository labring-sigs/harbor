package harbor

// Project represents a Harbor project returned by the API
type Project struct {
	ProjectID int64  `json:"project_id"`
	Name      string `json:"name"`
	Public    bool   `json:"public"`
}

// ProjectSpec is used for creating a new Harbor project
type ProjectSpec struct {
	Name         string
	Public       bool
	StorageLimit int64
	AutoScan     bool
}

// RobotSpec defines the desired state of a robot account
type RobotSpec struct {
	Name        string             `json:"name"`
	Duration    int64              `json:"duration"`
	Permissions []RobotPermission  `json:"permissions"`
}

// RobotPermission defines a single robot account permission
type RobotPermission struct {
	Kind      string        `json:"kind"`
	Namespace string        `json:"namespace"`
	Access    []RobotAccess `json:"access"`
}

// RobotAccess defines an access entry for a robot account
type RobotAccess struct {
	Action   string `json:"action"`
	Resource string `json:"resource,omitempty"`
}

// RobotAccount is the full robot account object returned by Harbor API.
// The creation response returns the credential in the "secret" field.
type RobotAccount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Token  string `json:"token,omitempty"`  // used by mock; real Harbor may send this
	Secret string `json:"secret,omitempty"` // real Harbor returns secret here
}

// RobotCredential is the response from the robot account creation API
// Deprecated: use RobotAccount instead
type RobotCredential struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}
