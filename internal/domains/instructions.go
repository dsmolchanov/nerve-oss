package domains

type DNSRecord struct {
	Type     string `json:"type"`
	Host     string `json:"host"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Purpose  string `json:"purpose"`
	Required bool   `json:"required"`
}

// DNSInstructions returns the DNS records required to verify ownership of a domain.
//
// Host values are relative to the domain's DNS zone (e.g. "_nerve-verify" not
// "_nerve-verify.example.com") to match how most DNS providers ask for records.
func DNSInstructions(verificationToken string) []DNSRecord {
	return []DNSRecord{
		{
			Type:     "TXT",
			Host:     OwnershipTXTLabel,
			Value:    verificationToken,
			Purpose:  "Domain ownership verification for Nerve",
			Required: true,
		},
	}
}
