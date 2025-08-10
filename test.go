// api/v1beta1/slice_types.go
type SliceSpec struct {
    Clusters []string      `json:"clusters"`
    Topology *TopologySpec `json:"topology,omitempty"`
}

type TopologySpec struct {
    Type               string                    `json:"type"`
    ConnectivityMatrix map[string][]string       `json:"connectivityMatrix,omitempty"`
    HubSpoke          *HubSpokeSpec             `json:"hubSpoke,omitempty"`
    VPNRoles          map[string]string         `json:"vpnRoles,omitempty"`
}

type HubSpokeSpec struct {
    Hub    string   `json:"hub"`
    Spokes []string `json:"spokes"`
}

type TopologyStatus struct {
    TunnelCount        int                       `json:"tunnelCount"`
    Validated         bool                      `json:"validated"`
    ConnectedClusters int                       `json:"connectedClusters"`
    ValidationMessage string                    `json:"validationMessage,omitempty"`
    EffectiveTopology map[string][]string       `json:"effectiveTopology,omitempty"`
}


// pkg/topology/calculator.go
type TopologyCalculator interface {
    CalculatePeers(slice *v1beta1.Slice, cluster string) ([]string, error)
    ValidateTopology(slice *v1beta1.Slice) error
    GetTunnelCount(slice *v1beta1.Slice) int
}

func (c *Calculator) CalculatePeers(slice *v1beta1.Slice, cluster string) ([]string, error) {
    if slice.Spec.Topology == nil {
        return c.calculateFullMeshPeers(slice.Spec.Clusters, cluster), nil
    }
    
    switch slice.Spec.Topology.Type {
    case "full-mesh":
        return c.calculateFullMeshPeers(slice.Spec.Clusters, cluster), nil
    case "hub-spoke":
        return c.calculateHubSpokePeers(slice.Spec.Topology.HubSpoke, cluster)
    case "partial-mesh":
        return c.calculatePartialMeshPeers(slice.Spec.Topology.ConnectivityMatrix, cluster)
    default:
        return nil, fmt.Errorf("unsupported topology type: %s", slice.Spec.Topology.Type)
    }
}

// controllers/slice_controller.go
func (r *SliceReconciler) reconcileSliceConfig(ctx context.Context, slice *v1beta1.Slice, cluster string) error {
    calculator := topology.NewCalculator()
    
    // Calculate topology-aware peers
    peers, err := calculator.CalculatePeers(slice, cluster)
    if err != nil {
        return fmt.Errorf("failed to calculate peers: %w", err)
    }
    
    // Get VPN role for cluster
    vpnRole := r.getVPNRole(slice, cluster)
    
    sliceConfig := &v1beta1.SliceConfig{
        Spec: v1beta1.SliceConfigSpec{
            SliceName: slice.Name,
            Peers:     peers,
            VPNRole:   vpnRole,
        },
    }
    
    return r.applySliceConfig(ctx, sliceConfig, cluster)
}

// worker-operator/controllers/sliceconfig_controller.go
func (r *SliceConfigReconciler) reconcileGateway(ctx context.Context, sliceConfig *v1beta1.SliceConfig) error {
    vpnRole := sliceConfig.Spec.VPNRole
    
    if vpnRole == "server" {
        return r.deployServerGateway(ctx, sliceConfig)
    } else if vpnRole == "client" {
        return r.deployClientGateway(ctx, sliceConfig)
    }
    
    return fmt.Errorf("invalid VPN role: %s", vpnRole)
}

func (r *SliceConfigReconciler) deployServerGateway(ctx context.Context, config *v1beta1.SliceConfig) error {
    gateway := &appsv1.Deployment{
        Spec: appsv1.DeploymentSpec{
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{{
                        Name:  "vpn-gateway",
                        Image: "kubeslice/gateway:latest",
                        Env: []corev1.EnvVar{
                            {Name: "VPN_ROLE", Value: "server"},
                            {Name: "ACCEPT_CONNECTIONS", Value: "true"},
                        },
                    }},
                },
            },
        },
    }
    return r.Client.Create(ctx, gateway)
}

func TestTopologyCalculation(t *testing.T) {
    tests := []struct {
        name        string
        slice       *v1beta1.Slice
        cluster     string
        expectedPeers []string
    }{
        {
            name: "hub-spoke topology - hub cluster",
            slice: createTestSlice("hub-spoke", map[string]interface{}{
                "hub": "cluster-hub",
                "spokes": []string{"cluster-1", "cluster-2"},
            }),
            cluster: "cluster-hub",
            expectedPeers: []string{"cluster-1", "cluster-2"},
        },
        {
            name: "hub-spoke topology - spoke cluster",
            slice: createTestSlice("hub-spoke", map[string]interface{}{
                "hub": "cluster-hub", 
                "spokes": []string{"cluster-1", "cluster-2"},
            }),
            cluster: "cluster-1",
            expectedPeers: []string{"cluster-hub"},
        },
    }
    
    calculator := topology.NewCalculator()
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            peers, err := calculator.CalculatePeers(tt.slice, tt.cluster)
            assert.NoError(t, err)
            assert.ElementsMatch(t, tt.expectedPeers, peers)
        })
    }
}

func TestSliceTopologyIntegration(t *testing.T) {
    testEnv := &envtest.Environment{
        CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd", "bases")},
    }
    
    cfg, err := testEnv.Start()
    require.NoError(t, err)
    defer testEnv.Stop()
    
    // Test hub-spoke slice creation
    slice := &v1beta1.Slice{
        ObjectMeta: metav1.ObjectMeta{Name: "test-hub-spoke"},
        Spec: v1beta1.SliceSpec{
            Clusters: []string{"hub", "spoke1", "spoke2"},
            Topology: &v1beta1.TopologySpec{
                Type: "hub-spoke",
                HubSpoke: &v1beta1.HubSpokeSpec{
                    Hub: "hub",
                    Spokes: []string{"spoke1", "spoke2"},
                },
            },
        },
    }
    
    err = k8sClient.Create(ctx, slice)
    require.NoError(t, err)
    
    // Verify SliceConfigs are created with correct peers
    eventually := func() bool {
        sliceConfigs := &v1beta1.SliceConfigList{}
        err := k8sClient.List(ctx, sliceConfigs)
        if err != nil || len(sliceConfigs.Items) != 3 {
            return false
        }
        
        // Verify hub has 2 peers, spokes have 1 peer each
        hubConfig := findSliceConfigForCluster(sliceConfigs, "hub")
        return len(hubConfig.Spec.Peers) == 2
    }
    
    assert.Eventually(t, eventually, time.Second*10, time.Millisecond*100)
}

func BenchmarkTopologyCalculation(b *testing.B) {
    clusters := make([]string, 50)
    for i := range clusters {
        clusters[i] = fmt.Sprintf("cluster-%d", i)
    }
    
    fullMeshSlice := createFullMeshSlice(clusters)
    hubSpokeSlice := createHubSpokeSlice(clusters[0], clusters[1:])
    
    calculator := topology.NewCalculator()
    
    b.Run("FullMesh-50-clusters", func(b *testing.B) {
        for i := 0; i < b.N; i++ {
            _, _ = calculator.CalculatePeers(fullMeshSlice, clusters[0])
        }
    })
    
    b.Run("HubSpoke-50-clusters", func(b *testing.B) {
        for i := 0; i < b.N; i++ {
            _, _ = calculator.CalculatePeers(hubSpokeSlice, clusters[0])
        }
    })
}