package api

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func podsGVR() schema.GroupVersionResource { return corev1.SchemeGroupVersion.WithResource("pods") }

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
